package server

import (
	"log/slog"
	"net/http"

	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/metrics"
)

// WithMetricsEndpoint mounts the scrape endpoint in front of the server's
// routes. It is a wrapper rather than a route on the webhook's mux so the
// caller decides whether metrics share the main listener at all.
func WithMetricsEndpoint(next http.Handler, scrape http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", scrape)
	mux.Handle("/", next)
	return mux
}

// MetricsHandler guards the scrape endpoint with the same admin credential as
// the observation deck. It is used when metrics share the main listener; a
// separate metrics listener serves the unguarded handler, because its address
// is what makes it reachable.
func MetricsHandler(m *metrics.Metrics, token string, logger *slog.Logger) http.Handler {
	return newAdminAuth(token, "rumil admin", logger).authorize(m.Handler().ServeHTTP)
}

// metricEvent keeps the event label bounded: GitHub sends a fixed set of event
// names, and anything else is folded into one series rather than becoming a
// label value the operator did not configure.
func metricEvent(event string) string {
	switch event {
	case eventPullRequest, eventIssueComment, eventReviewComment, eventPullRequestReview, eventReaction:
		return event
	case continuation.EventType:
		return "review_continuation"
	default:
		return "other"
	}
}
