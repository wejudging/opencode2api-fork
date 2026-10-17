package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const blockedExitCooldown = 12 * time.Hour

// OpenCode's free daily quota resets at 00:00 UTC (08:00 Asia/Shanghai).
// Every free-exit 429 cools that model for at most 12h, but never past reset.
func freeQuotaCooldown(now time.Time) time.Duration {
	utc := now.UTC()
	reset := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	return min(blockedExitCooldown, reset.Sub(now))
}

// Free inference restrictions are independent of transport/key health. The
// scope is upstream configuration + static proxy identity + model, shared by
// anonymous and authenticated free lanes. Never persist proxy credentials.
// Rotating #pool endpoints cannot identify an exit and deliberately opt out.
type freeExitState struct {
	ID         string    `json:"id"`
	Model      string    `json:"model"`
	Reason     string    `json:"reason"`
	Until      time.Time `json:"until"`
	Probing    bool      `json:"probing,omitempty"`
	generation uint64
}

type freeExitStore struct {
	mu      sync.Mutex
	entries map[string]*freeExitState
	path    string
	logger  *slog.Logger
	next    uint64
}

type freeExitLease struct {
	store      *freeExitStore
	id, model  string
	generation uint64
	once       sync.Once
}

func newFreeExitStore(path string, logger *slog.Logger) (*freeExitStore, error) {
	s := &freeExitStore{entries: make(map[string]*freeExitState), path: path, logger: logger}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var states []freeExitState
	if err := json.Unmarshal(b, &states); err != nil {
		return nil, err
	}
	for _, state := range states {
		// Keep recently expired entries to admit a single half-open probe after
		// a restart, but bound stale configuration/model accumulation.
		if state.Until.Before(time.Now().Add(-24 * time.Hour)) {
			continue
		}
		state.Probing = false
		s.next++
		state.generation = s.next
		s.entries[state.ID] = &state
	}
	return s, nil
}

func (g *Gateway) freeExitID(model string, proxy *proxyTransport) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{g.cfg.Upstream.Zen, g.cfg.Upstream.Go, g.cfg.Upstream.Anonymous, proxy.name, model}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) claimFreeExit(ctx context.Context, model string, proxy *proxyTransport) (*freeExitLease, time.Duration) {
	if g.freeExits == nil || proxy == nil || proxy.pool || isDiagnosticRequest(ctx) {
		return nil, 0
	}
	s := g.freeExits
	id := g.freeExitID(model, proxy)
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := &freeExitLease{store: s, id: id, model: model}
	if state := s.entries[id]; state != nil {
		if remaining := time.Until(state.Until); remaining > 0 {
			return nil, remaining
		}
		if state.Probing {
			return nil, time.Second
		}
		state.Probing = true
		lease.generation = state.generation
	}
	return lease, 0
}

// finish is idempotent. A deferred cancellation releases a half-open lease on
// every early return; actual results call it first. Old in-flight successes
// cannot erase a restriction installed by a newer response.
func (l *freeExitLease) finish(resp *http.Response, err error) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		s := l.store
		s.mu.Lock()
		defer s.mu.Unlock()
		state := s.entries[l.id]
		same := state != nil && state.generation == l.generation
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if same {
				state.Probing = false
			}
			return
		}
		delay := time.Duration(0)
		reason := ""
		observedAt := time.Now()
		if err == nil && resp != nil {
			if resp.StatusCode == http.StatusForbidden {
				delay, reason = blockedExitCooldown, "forbidden"
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, reason = freeQuotaCooldown(observedAt), "quota"
			}
		}
		changed := false
		if delay > 0 {
			until := observedAt.Add(delay)
			if state != nil && state.Until.After(until) {
				until = state.Until
			}
			if reason == "quota" && state != nil && state.Reason == "forbidden" && state.Until.After(observedAt) {
				// A concurrent quota response cannot downgrade a live 403 ban.
				reason = "forbidden"
			}
			if reason == "quota" {
				// A concurrent older response must not push a quota past reset.
				until = minTime(until, observedAt.Add(freeQuotaCooldown(observedAt)))
			}
			s.next++
			s.entries[l.id] = &freeExitState{ID: l.id, Model: l.model, Reason: reason, Until: until, generation: s.next}
			changed = true
		} else if same {
			if err == nil && resp != nil && resp.StatusCode/100 == 2 {
				delete(s.entries, l.id)
			} else {
				state.Probing = false
				state.Until = time.Now().Add(15 * time.Second)
			}
			changed = true
		}
		if changed {
			if err := s.saveLocked(); err != nil && s.logger != nil {
				s.logger.Warn("could not persist free exit restrictions", "event", "free_exit_persist_failed", "error", err)
			}
		}
	})
}

func (s *freeExitStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	for id, state := range s.entries {
		if !state.Probing && state.Until.Before(time.Now().Add(-24*time.Hour)) {
			delete(s.entries, id)
		}
	}
	states := make([]freeExitState, 0, len(s.entries))
	for _, state := range s.entries {
		states = append(states, *state)
	}
	b, err := json.Marshal(states)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".free-exits-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), s.path)
}

func (s *freeExitStore) snapshot() []freeExitState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]freeExitState, 0, len(s.entries))
	for _, state := range s.entries {
		if state.Until.After(time.Now().Add(-24 * time.Hour)) {
			states = append(states, *state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states
}

func freeCooldownResponse(delay time.Duration) *http.Response {
	seconds := max(1, int((delay+time.Second-1)/time.Second))
	return &http.Response{StatusCode: http.StatusTooManyRequests,
		Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {strconv.Itoa(seconds)}},
		Body:   io.NopCloser(strings.NewReader(`{"error":{"message":"No eligible free exit is ready; retry after the cooldown.","type":"rate_limit_error","code":"free_exit_cooldown"}}`)),
	}
}

func minCooldown(current, candidate time.Duration) time.Duration {
	if candidate > 0 && (current == 0 || candidate < current) {
		return candidate
	}
	return current
}

func isFreeExitRestriction(resp *http.Response, err error) bool {
	return err == nil && resp != nil && (resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (g *Gateway) maxTotalAttempts() int {
	if g.cfg.Retry.MaxTotalAttempts > 0 {
		return g.cfg.Retry.MaxTotalAttempts
	}
	return 6
}
