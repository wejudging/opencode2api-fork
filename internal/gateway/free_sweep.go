package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/models"
)

type freeSweepContextKey struct{}

// A sequential request owns this map. Each static exit/model may refund one
// confirmed 429, allowing an exhaustive quota sweep without increasing the
// ordinary error budget or making rotating #pool retries unbounded.
type freeSweep struct{ quotaAttempts map[string]bool }

func (g *Gateway) withFreeSweep(ctx context.Context, route models.Route) context.Context {
	if ctx.Value(freeSweepContextKey{}) != nil || isDiagnosticRequest(ctx) ||
		(!route.Anonymous && !g.catalog.IsFreeModel(route.ID)) {
		return ctx
	}
	return context.WithValue(ctx, freeSweepContextKey{}, &freeSweep{quotaAttempts: make(map[string]bool)})
}

func freeQuotaAttempts(ctx context.Context) int {
	if sweep, ok := ctx.Value(freeSweepContextKey{}).(*freeSweep); ok {
		return len(sweep.quotaAttempts)
	}
	return 0
}

func (g *Gateway) attemptBudgetAvailable(ctx context.Context, attempts int) bool {
	return attempts-freeQuotaAttempts(ctx) < g.maxTotalAttempts()
}

func (g *Gateway) recordFreeQuotaAttempt(ctx context.Context, model string, proxy *proxyTransport, resp *http.Response, err error) {
	sweep, ok := ctx.Value(freeSweepContextKey{}).(*freeSweep)
	if ok && proxy != nil && !proxy.pool && err == nil && resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		sweep.quotaAttempts[g.freeExitID(model, proxy)] = true
	}
}

// A 429 may only describe the whole free route when every configured route
// candidate has a live 429 restriction. A skipped/unhealthy/banned/unverified
// exit is not proof of quota exhaustion. Its result is 503, not a false 429.
func (g *Gateway) finalizeFreeQuotaResponse(ctx context.Context, route models.Route, resp *http.Response) *http.Response {
	if ctx.Value(freeSweepContextKey{}) == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp
	}
	candidates := make(map[*proxyTransport]bool)
	if route.Anonymous {
		for _, node := range g.anonymous.nodes {
			candidates[node.proxy] = true
		}
	} else {
		tiers := route.KeyTiers
		if len(tiers) == 0 {
			tiers = []config.Tier{route.Tier}
		}
		for _, tier := range tiers {
			nodes := g.zenNodes
			if tier == config.TierGo {
				nodes = g.goNodes
			}
			if nodes != nil {
				for _, node := range nodes.nodes {
					candidates[nodes.Proxy(node)] = true
				}
			}
		}
	}
	allQuota := len(candidates) > 0 && g.freeExits != nil
	var earliest time.Duration
	if g.freeExits != nil {
		g.freeExits.mu.Lock()
		for proxy := range candidates {
			if proxy == nil || proxy.pool {
				allQuota = false
				continue
			}
			state := g.freeExits.entries[g.freeExitID(route.ID, proxy)]
			if state == nil || state.Reason != "quota" || !state.Until.After(time.Now()) {
				allQuota = false
				continue
			}
			earliest = minCooldown(earliest, time.Until(state.Until))
		}
		g.freeExits.mu.Unlock()
	}
	httpx.DrainAndClose(resp.Body)
	if allQuota {
		return freeCooldownResponse(earliest)
	}
	return &http.Response{StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"error":{"message":"Free exit scan incomplete: at least one exit is unavailable or unverified; not all exits are confirmed rate-limited.","type":"service_unavailable","code":"free_exit_scan_incomplete"}}`)),
	}
}
