package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func TestAttemptTimeoutPreservesRequestBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	req, _ := http.NewRequestWithContext(parent, "POST", "http://unused.invalid/v1/chat/completions", nil)
	_, err := doInferenceAttempt(client, req, 0, 10*time.Millisecond)
	if !errors.Is(err, errAttemptTimeout) || parent.Err() != nil {
		t.Fatalf("attempt=%v parent=%v", err, parent.Err())
	}
	client.Transport = testRoundTripper(func(*http.Request) (*http.Response, error) { return testResponse(200), nil })
	resp, err := doInferenceAttempt(client, req, 0, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
func TestFirstEventTimeoutBeforeCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL, nil)
	_, err := doInferenceAttempt(srv.Client(), req, 20*time.Millisecond, time.Second)
	if !errors.Is(err, errFirstEventTimeout) || !upstreamAccepted(err) {
		t.Fatalf("wrong timeout classification: %v", err)
	}
}
func TestCommittedStreamSurvivesAttemptDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"text\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(100 * time.Millisecond):
			io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	meta := &telemetry.RequestMeta{}
	ctx, cancel := context.WithTimeout(telemetry.WithRequestMeta(context.Background(), meta), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, nil)
	resp, err := doInferenceAttempt(srv.Client(), req, 50*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), "[DONE]") || meta.FirstEventMS <= 0 {
		t.Fatalf("stream interrupted or missing timing: %q %v %+v", body, err, meta)
	}
}
func TestGoogleWireEndpointsAndCredentials(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := []byte(`{"model":"gemini-test","contents":[],"stream":false}`)
		if stream {
			body = []byte(`{"model":"gemini-test","contents":[],"stream":true}`)
		}
		req, err := newUpstreamRequest(context.Background(), "https://provider.invalid/zen/go", wire.Google, body, identity.RequestIDs{}, "test-key")
		if err != nil {
			t.Fatal(err)
		}
		want := ":generateContent"
		if stream {
			want = ":streamGenerateContent?alt=sse"
		}
		if !strings.HasSuffix(req.URL.String(), want) || !strings.Contains(req.URL.Path, "/inference/go/v1/models/gemini-test") || req.Header.Get("x-goog-api-key") != "test-key" {
			t.Fatalf("wrong Google request: %s", req.URL)
		}
		b, _ := io.ReadAll(req.Body)
		if strings.Contains(string(b), `"stream"`) || strings.Contains(string(b), `"model"`) {
			t.Fatalf("internal fields leaked: %s", b)
		}
	}
}
