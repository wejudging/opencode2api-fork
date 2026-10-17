package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func sweepGateway(t *testing.T, count int, status func(int) int) (*Gateway, []int) {
	t.Helper()
	calls := make([]int, count)
	handlers := make([]testRoundTripper, count)
	for i := range handlers {
		handlers[i] = func(*http.Request) (*http.Response, error) {
			calls[i]++
			resp := testResponse(status(i))
			resp.Header.Del("Retry-After") // even an unadorned 429 must cool/refund
			return resp, nil
		}
	}
	return testGateway(t, handlers...), calls
}

func TestFreeSweepReachesSuccessBeyondSixthExit(t *testing.T) {
	for _, successExit := range []int{6, 10} {
		t.Run(fmt.Sprint(successExit+1), func(t *testing.T) {
			g, calls := sweepGateway(t, 11, func(i int) int {
				if i == successExit {
					return 200
				}
				return 429
			})
			g.cfg.Upstream.Anonymous = "https://unused.invalid/inference"
			route, bodies := testRoute()
			resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
			if err != nil || resp == nil {
				t.Fatalf("response: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 || attempts != successExit+1 {
				t.Fatalf("status=%d attempts=%d calls=%v", resp.StatusCode, attempts, calls)
			}
			for i, n := range calls {
				want := 0
				if i <= successExit {
					want = 1
				}
				if n != want {
					t.Fatalf("repeated/skipped exit %d: %v", i, calls)
				}
			}
		})
	}
}

func TestFreeSweepAllEleven429ThenZeroAttemptCooldown(t *testing.T) {
	g, calls := sweepGateway(t, 11, func(int) int { return 429 })
	g.cfg.Upstream.Anonymous = "https://unused.invalid/inference"
	route, bodies := testRoute()
	for _, wantAttempts := range []int{11, 0} {
		resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
		if err != nil || resp == nil {
			t.Fatalf("response: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 429 || attempts != wantAttempts || !strings.Contains(string(body), "free_exit_cooldown") || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("status=%d attempts=%d body=%s", resp.StatusCode, attempts, body)
		}
	}
	for i, n := range calls {
		if n != 1 {
			t.Fatalf("exit %d was retried: %v", i, calls)
		}
	}
}

func TestFreeSweepSkipsSixKnownQuotasAndChecksRemainingFive(t *testing.T) {
	g, calls := sweepGateway(t, 11, func(int) int { return 429 })
	route, bodies := testRoute()
	for _, p := range g.transports.items[:6] {
		blockExit(g, route.ID, p, 429)
	}
	resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || attempts != 5 {
		t.Fatalf("status=%d attempts=%d calls=%v", resp.StatusCode, attempts, calls)
	}
	for i, n := range calls {
		want := 1
		if i < 6 {
			want = 0
		}
		if n != want {
			t.Fatalf("calls=%v", calls)
		}
	}
}

func TestFreeKeySweepBypassesThreeAndSixAttemptCapsFor429(t *testing.T) {
	g, calls := sweepGateway(t, 11, func(int) int { return 429 })
	keys := make([]string, 11)
	for i := range keys {
		keys[i] = fmt.Sprintf("test-key-%d", i)
	}
	g.zenNodes, _ = newNodePool(keys, g.transports, time.Second)
	route, bodies := testRoute()
	route.Anonymous = false
	resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || attempts != 11 {
		t.Fatalf("status=%d attempts=%d calls=%v", resp.StatusCode, attempts, calls)
	}
	for _, node := range g.zenNodes.nodes {
		if node.cooldownUntil.Load() != 0 {
			t.Fatal("free quota cooled paid-model key")
		}
	}
}

func TestIncompleteFreeSweepDoesNotClaimAll429(t *testing.T) {
	for _, kind := range []string{"unhealthy", "forbidden", "server_error"} {
		t.Run(kind, func(t *testing.T) {
			g, _ := sweepGateway(t, 11, func(i int) int {
				if i == 0 && kind == "server_error" {
					return 500
				}
				return 429
			})
			route, bodies := testRoute()
			route.KeyTiers = nil // isolate unavailable anonymous exits from key fallback
			if kind == "unhealthy" {
				g.transports.items[0].healthy.Store(false)
			}
			if kind == "forbidden" {
				blockExit(g, route.ID, g.transports.items[0], 403)
			}
			resp, _, _, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
			if err != nil || resp == nil {
				t.Fatalf("response: %v", err)
			}
			defer resp.Body.Close()
			w := httptest.NewRecorder()
			copyErrorResponse(w, wire.Chat, resp, "test-incomplete")
			if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"free_exit_scan_incomplete"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestFreeSweepKeepsOrdinaryFailureBudget(t *testing.T) {
	g, _ := sweepGateway(t, 11, func(int) int { return 500 })
	route, bodies := testRoute()
	resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 6 || resp.StatusCode != 500 {
		t.Fatalf("status=%d attempts=%d", resp.StatusCode, attempts)
	}
}

func TestPaid429KeepsExistingAttemptBudget(t *testing.T) {
	g, _ := sweepGateway(t, 11, func(int) int { return 429 })
	keys := make([]string, 11)
	for i := range keys {
		keys[i] = fmt.Sprintf("paid-test-%d", i)
	}
	g.zenNodes, _ = newNodePool(keys, g.transports, time.Second)
	route := models.Route{ID: "paid-model", Tier: config.TierZen, Protocol: wire.Chat, KeyTiers: []config.Tier{config.TierZen}}
	_, bodies := testRoute()
	resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 3 || len(g.freeExits.snapshot()) != 0 {
		t.Fatalf("attempts=%d", attempts)
	}
}

func TestCanceledSweepDoesNotReturnPrevious429(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, _ := sweepGateway(t, 11, func(int) int { return 429 })
	g.transports.items[1].client.Transport = testRoundTripper(func(*http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled })
	route, bodies := testRoute()
	resp, _, _, err := g.doUpstreamTiers(ctx, route, bodies, identity.RequestIDs{}, 0)
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

func TestFree429CooldownCappedByTwelveHoursAndUTCReset(t *testing.T) {
	for _, tc := range []struct {
		now  string
		want time.Duration
	}{
		{"2026-10-18T01:00:00Z", 12 * time.Hour},
		{"2026-10-18T20:00:00Z", 4 * time.Hour},
		{"2026-10-18T23:59:59Z", time.Second},
		{"2026-10-19T00:00:00Z", 12 * time.Hour},
		{"2026-10-19T04:00:00+08:00", 4 * time.Hour},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := freeQuotaCooldown(now); got != tc.want {
			t.Fatalf("%s got=%s want=%s", tc.now, got, tc.want)
		}
	}
	for _, retry := range []string{"", "1", "53000", "999999"} {
		g := testGateway(t, nil)
		p := g.transports.items[0]
		lease, _ := g.claimFreeExit(context.Background(), "test-free", p)
		resp := testResponse(429)
		resp.Header.Set("Retry-After", retry)
		lease.finish(resp, nil)
		resp.Body.Close()
		state := g.freeExits.snapshot()[0]
		want := freeQuotaCooldown(time.Now())
		if state.Reason != "quota" || time.Until(state.Until) < want-time.Second || time.Until(state.Until) > want+time.Second {
			t.Fatalf("retry=%q state=%+v", retry, state)
		}
	}
}

func TestConcurrentQuotaCannotShortenForbiddenBan(t *testing.T) {
	g := testGateway(t, nil)
	p := g.transports.items[0]
	older, _ := g.claimFreeExit(context.Background(), "model-free", p)
	blockExit(g, "model-free", p, 403)
	older.finish(testResponse(429), nil)
	state := g.freeExits.snapshot()[0]
	if state.Reason != "forbidden" || time.Until(state.Until) < 11*time.Hour {
		t.Fatalf("ban downgraded: %+v", state)
	}
}

func TestRotatingPoolDoesNotRefundOrClaimAll429(t *testing.T) {
	g, calls := sweepGateway(t, 1, func(int) int { return 429 })
	g.transports.items[0].pool = true
	g.anonymous = newAnonymousPool(true, g.transports, time.Second)
	route, bodies := testRoute()
	route.KeyTiers = nil
	resp, _, attempts, err := g.doUpstreamTiers(context.Background(), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 || attempts != 3 || calls[0] != 3 || len(g.freeExits.snapshot()) != 0 {
		t.Fatalf("status=%d attempts=%d calls=%v", resp.StatusCode, attempts, calls)
	}
}

func TestDiagnosticSweepDoesNotChangeStateOrRefund(t *testing.T) {
	g, _ := sweepGateway(t, 11, func(int) int { return 429 })
	route, bodies := testRoute()
	resp, _, attempts, err := g.doUpstreamTiers(WithDiagnosticRequest(context.Background()), route, bodies, identity.RequestIDs{}, 0)
	if err != nil || resp == nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 6 || len(g.freeExits.snapshot()) != 0 {
		t.Fatalf("diagnostic changed state/budget: attempts=%d", attempts)
	}
}
