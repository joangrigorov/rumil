package server

import (
	"log/slog"
	"net/http"

	"github.com/joangrigorov/rumil/internal/config"
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

// MetricsHandler guards the scrape endpoint with the same admin users as the
// observation deck; a scraper uses HTTP Basic. It is used when metrics share
// the main listener; a separate metrics listener serves the unguarded handler,
// because its address is what makes it reachable.
func MetricsHandler(m *metrics.Metrics, users []config.AdminUser, logger *slog.Logger) http.Handler {
	return MetricsHandlerSource(m, func() []config.AdminUser { return users }, logger)
}

// MetricsHandlerSource is MetricsHandler with a user list that may change
// between requests.
func MetricsHandlerSource(m *metrics.Metrics, users func() []config.AdminUser, logger *slog.Logger) http.Handler {
	return NewAdminAuthSource(users, logger).authorize(m.Handler().ServeHTTP)
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
