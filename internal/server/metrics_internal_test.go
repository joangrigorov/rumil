package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/metrics"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// The observer turns transcript events into counters without a database or a
// writer, which is what makes the mapping testable on its own.
func TestSessionObserverCountsProviderAndFallbackEvents(t *testing.T) {
	m := metrics.New()
	o := &sessionObserver{meter: m, events: make(chan store.ReviewEvent, 16)}
	ctx := context.Background()

	o.Observe(ctx, reviewer.Event{Type: "step_finish", Payload: map[string]any{}})
	o.Observe(ctx, reviewer.Event{Type: "error", Payload: map[string]any{
		"class": "rate_limit", "header_timeout": true,
	}})
	o.Observe(ctx, reviewer.Event{Type: "retry", Payload: map[string]any{}})
	o.Observe(ctx, reviewer.Event{Type: "snapshot_failed", Payload: map[string]any{}})
	o.Observe(ctx, reviewer.Event{Type: "memory_failed", Payload: map[string]any{}})

	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	text := recorder.Body.String()

	for _, want := range []string{
		`rumil_provider_requests_total{class="ok"} 1`,
		`rumil_provider_requests_total{class="rate_limit"} 1`,
		`rumil_provider_header_timeouts_total 1`,
		`rumil_provider_retries_total 1`,
		`rumil_snapshot_fallbacks_total 1`,
		`rumil_memory_retrieval_failures_total 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition does not carry %q:\n%s", want, text)
		}
	}
}
