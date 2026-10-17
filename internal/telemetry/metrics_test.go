package telemetry

import (
	"testing"
	"time"
)

func TestCanceledRequestsAreSeparateFromFailures(t *testing.T) {
	m := NewMonitor()
	for _, input := range []struct {
		status  int
		outcome string
	}{
		{200, ""}, {200, "client_canceled"}, {429, ""}, {200, "stream_error"},
	} {
		m.Record("/v1/chat/completions", input.status, time.Second, &RequestMeta{Model: "model", Channel: "key", ServerKeyID: "caller", Outcome: input.outcome})
	}
	s := m.Snapshot()
	for _, summary := range []MetricSummary{s.Window, s.Lifetime} {
		if summary.Total != 4 || summary.Success != 1 || summary.Errors != 2 || summary.ClientCanceled != 1 || summary.SuccessRate != 1.0/3 {
			t.Fatalf("wrong summary: %+v", summary)
		}
	}
	if s.Outcomes["client_canceled"] != 1 || s.Outcomes["stream_error"] != 1 || s.ModelOutcomes["model"]["client_error"] != 1 || s.CallerOutcomes["caller"]["success"] != 1 {
		t.Fatalf("missing outcome dimensions: %+v", s.Outcomes)
	}
}
