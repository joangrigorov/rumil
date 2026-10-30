package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/metrics"
	"github.com/joangrigorov/rumil/internal/store"
)

type fakeSource struct {
	queue    store.QueueState
	sessions store.SessionState
	err      error
}

func (f fakeSource) QueueState(context.Context) (store.QueueState, error) {
	return f.queue, f.err
}

func (f fakeSource) SessionState(context.Context) (store.SessionState, error) {
	return f.sessions, f.err
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	return recorder.Body.String()
}

// Every counter and the scrape-time gauges belong in the text exposition. The
// names are the ones the README documents, so a rename has to be deliberate.
func TestMetricsRenderTheTextFormat(t *testing.T) {
	m := metrics.New()
	cost := 0.5

	m.Job("pull_request", "received")
	m.Job("pull_request", "queued")
	m.Job("pull_request", "completed")
	m.Job("issue_comment", "failed")
	m.ReviewDuration("submitted", 2*time.Second)
	m.ProviderRequest("ok")
	m.ProviderRequest("rate_limit")
	m.ProviderRetry()
	m.ProviderHeaderTimeout()
	m.ReviewWithoutSubmission("turn_limit")
	m.SnapshotFallback()
	m.MemoryFailure()
	m.Usage(10, 5, 2, 1, &cost)
	m.CollectDatabase(fakeSource{
		queue:    store.QueueState{Queued: 3, Running: 1, OldestQueued: 90 * time.Second},
		sessions: store.SessionState{Running: 2, Succeeded: 7, Failed: 1},
	}, nil)

	text := scrape(t, m)
	wants := []string{
		`rumil_webhook_jobs_total{event="pull_request",outcome="received"} 1`,
		`rumil_webhook_jobs_total{event="pull_request",outcome="queued"} 1`,
		`rumil_webhook_jobs_total{event="pull_request",outcome="completed"} 1`,
		`rumil_webhook_jobs_total{event="issue_comment",outcome="failed"} 1`,
		`rumil_review_duration_seconds_count{outcome="submitted"} 1`,
		`rumil_review_duration_seconds_sum{outcome="submitted"} 2`,
		`rumil_provider_requests_total{class="ok"} 1`,
		`rumil_provider_requests_total{class="rate_limit"} 1`,
		`rumil_provider_retries_total 1`,
		`rumil_provider_header_timeouts_total 1`,
		`rumil_reviews_without_submission_total{reason="turn_limit"} 1`,
		`rumil_snapshot_fallbacks_total 1`,
		`rumil_memory_retrieval_failures_total 1`,
		`rumil_tokens_total{kind="input"} 10`,
		`rumil_tokens_total{kind="output"} 5`,
		`rumil_tokens_total{kind="cache_read"} 2`,
		`rumil_tokens_total{kind="cache_creation"} 1`,
		`rumil_estimated_cost_usd_total 0.5`,
		`rumil_queue_jobs{status="queued"} 3`,
		`rumil_queue_jobs{status="running"} 1`,
		`rumil_queue_oldest_job_age_seconds 90`,
		`rumil_sessions{status="running"} 2`,
		`rumil_sessions{status="succeeded"} 7`,
		`rumil_sessions{status="failed"} 1`,
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition does not carry %q:\n%s", want, text)
		}
	}
}

// A database that cannot answer leaves the gauges out and counts the scrape
// error, rather than failing the whole exposition.
func TestMetricsTolerateADatabaseReadFailure(t *testing.T) {
	m := metrics.New()
	m.CollectDatabase(fakeSource{err: errors.New("database is gone")}, nil)

	text := scrape(t, m)
	// Both reads failed: the queue and the sessions.
	if !strings.Contains(text, `rumil_metrics_scrape_errors_total 2`) {
		t.Errorf("the exposition does not carry the scrape errors:\n%s", text)
	}
	if strings.Contains(text, `rumil_queue_jobs{`) {
		t.Errorf("the exposition carries a queue gauge read from a failed query:\n%s", text)
	}
}

// A nil set is what a component built without metrics holds, and recording
// through it must not panic.
func TestNilMetricsAcceptRecords(t *testing.T) {
	var m *metrics.Metrics
	m.Job("pull_request", "received")
	m.ReviewDuration("submitted", time.Second)
	m.ProviderRequest("ok")
	m.ProviderRetry()
	m.ProviderHeaderTimeout()
	m.ReviewWithoutSubmission("turn_limit")
	m.SnapshotFallback()
	m.MemoryFailure()
	m.Usage(1, 1, 0, 0, nil)
}
