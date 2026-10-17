package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testResponse(status int) *http.Response {
	r := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"big-pickle"}]}`))}
	if status == 429 {
		r.Header.Set("Retry-After", "53000")
	}
	return r
}
func testGateway(t *testing.T, handlers ...testRoundTripper) *Gateway {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := &transportPool{}
	for i, handler := range handlers {
		proxy := &proxyTransport{index: i, name: string(rune('a' + i)), client: &http.Client{Transport: handler}}
		proxy.healthy.Store(true)
		pool.items = append(pool.items, proxy)
	}
	store, err := newFreeExitStore("", logger)
	if err != nil {
		t.Fatal(err)
	}
	zen, _ := newNodePool([]string{"test-key-a", "test-key-b"}, pool, 15*time.Second)
	goNodes, _ := newNodePool(nil, pool, 15*time.Second)
	return &Gateway{
		cfg:        config.Config{Upstream: config.UpstreamConfig{Zen: models.LegacyZenBase, Anonymous: models.LegacyZenBase}, Retry: config.RetryConfig{MaxAttempts: 3, MaxTotalAttempts: 6}},
		transports: pool, anonymous: newAnonymousPool(true, pool, 15*time.Second), freeExits: store,
		logger: logger, zenNodes: zen, goNodes: goNodes, catalog: models.NewCatalog(config.TierZen, nil),
	}
}
func testRoute() (models.Route, map[config.Tier][]byte) {
	return models.Route{ID: "model-free", Tier: config.TierZen, Protocol: wire.Chat, Anonymous: true, KeyTiers: []config.Tier{config.TierZen}},
		map[config.Tier][]byte{config.TierZen: []byte(`{"model":"model-free","messages":[{"role":"user","content":"test"}]}`)}
}
func blockExit(g *Gateway, model string, proxy *proxyTransport, status int) {
	lease, _ := g.claimFreeExit(context.Background(), model, proxy)
	lease.finish(testResponse(status), nil)
}

func TestAnonymousCursorNeverBypassesCooldown(t *testing.T) {
	g := testGateway(t, nil)
	g.anonymous.nodes[0].cooldownUntil.Store(time.Now().Add(12 * time.Hour).UnixNano())
	cursor := g.anonymous.CursorFor("")
	if cursor.Next() != nil {
		t.Fatal("selected a cooling exit")
	}
}
func TestQuotaOnOneExitStillTriesOtherExit(t *testing.T) {
	var second int
	g := testGateway(t, func(*http.Request) (*http.Response, error) { return testResponse(429), nil }, func(*http.Request) (*http.Response, error) { second++; return testResponse(200), nil })
	route, bodies := testRoute()
	resp, err, n := g.doAnonymousUpstream(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || n != 2 || second != 1 {
		t.Fatalf("status=%d attempts=%d second=%d", resp.StatusCode, n, second)
	}
}
func TestForbiddenAndQuotaPersistAcrossDiscoveryAndRecovery(t *testing.T) {
	for _, status := range []int{403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g := testGateway(t, func(*http.Request) (*http.Response, error) { return testResponse(200), nil })
			p := g.transports.items[0]
			blockExit(g, "big-pickle", p, status)
			g.refreshAnonymousTier(context.Background(), models.LegacyZenBase)
			g.restoreProxy(p)
			if _, wait := g.claimFreeExit(context.Background(), "big-pickle", p); wait < 11*time.Hour {
				t.Fatalf("restriction erased: %s", wait)
			}
			if lease, wait := g.claimFreeExit(context.Background(), "other-free-model", p); lease == nil || wait != 0 {
				t.Fatal("different model was blocked")
			}
		})
	}
}
func TestFreeRestrictionsSharedWithKeysButDoNotCoolPaidKeys(t *testing.T) {
	var calls int
	g := testGateway(t, func(*http.Request) (*http.Response, error) { calls++; return testResponse(429), nil })
	route, bodies := testRoute()
	route.Anonymous = false
	resp, err, n := g.doKeyUpstream(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("err=%v", err)
	}
	resp.Body.Close()
	if calls != 1 || n != 1 {
		t.Fatalf("same exhausted exit retried through another key: calls=%d attempts=%d", calls, n)
	}
	for _, node := range g.zenNodes.nodes {
		if node.cooldownUntil.Load() != 0 {
			t.Fatal("free quota cooled a paid-model key")
		}
	}
	resp, err, n = g.doAnonymousUpstream(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil || n != 0 || calls != 1 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("anonymous did not fail fast: n=%d calls=%d err=%v", n, calls, err)
	}
	resp.Body.Close()
	// The paid route uses the same account and exit, ignoring the free ledger.
	g.transports.items[0].client.Transport = testRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return testResponse(200), nil })
	route.ID = "paid-model"
	resp, err, n = g.doKeyUpstream(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp.StatusCode != 200 || n != 1 {
		t.Fatalf("paid route blocked: %v", err)
	}
	resp.Body.Close()
}
func TestTotalAttemptBudgetAcrossAnonymousKeysAndOrigins(t *testing.T) {
	var calls int
	h := func(*http.Request) (*http.Response, error) { calls++; return testResponse(403), nil }
	g := testGateway(t, h, h, h, h, h, h, h)
	g.cfg.Upstream.Anonymous = "https://unused.invalid/inference"
	g.cfg.Retry.MaxTotalAttempts = 5
	route, bodies := testRoute()
	resp, _, n, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("err=%v", err)
	}
	resp.Body.Close()
	if n != 5 || calls != 5 {
		t.Fatalf("budget exceeded: attempts=%d calls=%d", n, calls)
	}
}
func TestFreeExitPersistenceAndSingleHalfOpenProbe(t *testing.T) {
	g := testGateway(t, nil)
	path := filepath.Join(t.TempDir(), "state.json")
	g.freeExits.path = path
	p := g.transports.items[0]
	p.name = "socks5://private-user:private-pass@proxy.invalid:1080"
	blockExit(g, "big-pickle", p, 403)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-pass") || strings.Contains(string(b), "proxy.invalid") {
		t.Fatal("credential/address leaked into state")
	}
	reloaded, err := newFreeExitStore(path, g.logger)
	if err != nil {
		t.Fatal(err)
	}
	g.freeExits = reloaded
	if _, wait := g.claimFreeExit(context.Background(), "big-pickle", p); wait < 11*time.Hour {
		t.Fatal("restart lost cooldown")
	}
	id := g.freeExitID("big-pickle", p)
	reloaded.entries[id].Until = time.Now().Add(-time.Second)
	var admitted atomic.Int32
	var lease *freeExitLease
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, wait := g.claimFreeExit(context.Background(), "big-pickle", p)
			if l != nil && wait == 0 {
				admitted.Add(1)
				lease = l
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("half-open admitted=%d", admitted.Load())
	}
	lease.finish(testResponse(200), nil)
	if len(reloaded.snapshot()) != 0 {
		t.Fatal("successful probe did not clear state")
	}
}
func TestOlderSuccessCannotEraseNewBan(t *testing.T) {
	g := testGateway(t, nil)
	p := g.transports.items[0]
	old, _ := g.claimFreeExit(context.Background(), "big-pickle", p)
	blockExit(g, "big-pickle", p, 403)
	old.finish(testResponse(200), nil)
	if _, wait := g.claimFreeExit(context.Background(), "big-pickle", p); wait == 0 {
		t.Fatal("old success erased new ban")
	}
}
func TestCanceledProbeReleasesLeaseAndRotatingPoolsOptOut(t *testing.T) {
	g := testGateway(t, nil)
	p := g.transports.items[0]
	blockExit(g, "big-pickle", p, 403)
	g.freeExits.entries[g.freeExitID("big-pickle", p)].Until = time.Now().Add(-time.Second)
	l, _ := g.claimFreeExit(context.Background(), "big-pickle", p)
	l.finish(nil, context.Canceled)
	if l, wait := g.claimFreeExit(context.Background(), "big-pickle", p); l == nil || wait != 0 {
		t.Fatal("canceled probe leaked lease")
	}
	p.pool = true
	if l, wait := g.claimFreeExit(context.Background(), "big-pickle", p); l != nil || wait != 0 {
		t.Fatal("rotating pool treated as one static exit")
	}
}
func TestRuntimeRebuildSharesFreeExitState(t *testing.T) {
	g := testGateway(t, nil)
	manager := &RuntimeManager{logger: g.logger, freeExits: g.freeExits}
	cfg := config.Config{Proxies: []string{"direct"}, Retry: config.RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300}}
	runtime, err := manager.build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.gateway.freeExits != g.freeExits {
		t.Fatal("configuration rebuild replaced ledger")
	}
}
