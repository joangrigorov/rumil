package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/metrics"
	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", recorder.Code)
	}
	return recorder.Body.String()
}

// On the main listener the endpoint is an operator surface, so it needs the
// admin credential like the deck and the moderation page.
func TestMetricsOnTheMainListenerNeedsTheAdminToken(t *testing.T) {
	m := metrics.New()
	handler := server.WithMetricsEndpoint(http.NotFoundHandler(),
		server.MetricsHandler(m, "secret-token", slog.New(slog.DiscardHandler)))

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without a credential = %d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status with the token = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "# HELP rumil_") {
		t.Errorf("the answer is not a Prometheus exposition:\n%s", recorder.Body.String())
	}
}

// An unset admin token closes the endpoint rather than opening it, which is
// the rule every other admin surface follows.
func TestMetricsOnTheMainListenerRefusesWithoutAConfiguredToken(t *testing.T) {
	m := metrics.New()
	handler := server.WithMetricsEndpoint(http.NotFoundHandler(),
		server.MetricsHandler(m, "", slog.New(slog.DiscardHandler)))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no admin token is configured", recorder.Code)
	}
}

// A delivery moves the received and queued counters, and a retried delivery
// moves only received.
func TestWebhookDeliveriesMoveTheJobCounters(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})

	m := metrics.New()
	webhook := server.NewWebhook(h.cfg.GitHub.WebhookSecret, h.cfg.App.BotHandle,
		h.cfg.GitHub.InstallationID, h.db, h.logger, server.WithWebhookMetrics(m))
	handler := server.Handler(webhook, h.db, nil)

	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	first := h.deliver(t, handler, "pull_request", "d-metrics-delivery", body, sign(webhookSecret, body))
	if first.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", first.Code, first.Body.String())
	}
	second := h.deliver(t, handler, "pull_request", "d-metrics-delivery", body, sign(webhookSecret, body))
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200 (body: %s)", second.Code, second.Body.String())
	}

	text := scrapeMetrics(t, m)
	for _, want := range []string{
		`rumil_webhook_jobs_total{event="pull_request",outcome="received"} 2`,
		`rumil_webhook_jobs_total{event="pull_request",outcome="queued"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition does not carry %q:\n%s", want, text)
		}
	}
}

// A whole review moves the completion counter, the duration histogram, the
// token counter and the database-backed session gauge.
func TestACompletedReviewMovesTheReviewCounters(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("get_pr_metadata", nil)
	llm.QueueToolCall("get_pr_diff", nil)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})
	m := metrics.New()
	m.CollectDatabase(h.db, h.logger)

	webhook := server.NewWebhook(h.cfg.GitHub.WebhookSecret, h.cfg.App.BotHandle,
		h.cfg.GitHub.InstallationID, h.db, h.logger, server.WithWebhookMetrics(m))
	handler := server.Handler(webhook, h.db, nil)
	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	if response := h.deliver(t, handler, "pull_request", "d-metrics-review", body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	pool := server.NewPool(h.db, h.engine(t, gh), h.cfg.App.BotHandle, 1, h.logger, server.WithMetrics(m))
	sessionsUI := server.NewSessionsUI(h.db, "admin-token", h.logger, server.WithSessionsMetrics(m))
	pool.SetSessionRecorder(sessionsUI.Recorder())

	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)
	waitFor(t, "the review to finish", jobCompleted(t, h, "d-metrics-review"))
	stopPool()
	pool.Wait()

	text := scrapeMetrics(t, m)
	for _, want := range []string{
		`rumil_webhook_jobs_total{event="pull_request",outcome="completed"} 1`,
		`rumil_review_duration_seconds_count{outcome="submitted"} 1`,
		`rumil_sessions{status="succeeded"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition does not carry %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `rumil_tokens_total{kind="input"}`) {
		t.Errorf("the exposition carries no input tokens:\n%s", text)
	}
}
