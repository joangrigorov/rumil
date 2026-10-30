// Package metrics is the Prometheus surface of `rumil serve`: webhook jobs, the
// queue, review sessions, review latency, provider failures, snapshot and memory
// fallbacks, and token usage.
//
// Labels stay low-cardinality on purpose. Nothing records a repository, a pull
// request number, a session id, a tool argument or an error string; an operator
// who needs those reads the deck or the log, and a metrics store does not grow
// a series per pull request.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/joangrigorov/rumil/internal/store"
)

// Namespace prefixes every metric.
const Namespace = "rumil"

// scrapeTimeout bounds the database reads behind one scrape. A scrape that
// cannot read leaves the series absent rather than holding the registry open.
const scrapeTimeout = 5 * time.Second

// Metrics is one registry and the collectors over it. Each process builds one;
// tests build one per test, so a count is never inherited from another run.
type Metrics struct {
	registry *prometheus.Registry

	jobs                     *prometheus.CounterVec
	reviewDuration           *prometheus.HistogramVec
	providerRequests         *prometheus.CounterVec
	providerRetries          prometheus.Counter
	providerHeaderTimeouts   prometheus.Counter
	reviewsWithoutSubmission *prometheus.CounterVec
	snapshotFallbacks        prometheus.Counter
	memoryFailures           prometheus.Counter
	tokens                   *prometheus.CounterVec
	estimatedCost            prometheus.Counter
}

// New builds the registry and its counters.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		jobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "webhook_jobs_total",
			Help:      "Webhook deliveries by event type and outcome (received, queued, completed, failed).",
		}, []string{"event", "outcome"}),
		reviewDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "review_duration_seconds",
			Help:      "Wall-clock time a review ran, by whether it submitted a review.",
			Buckets:   []float64{10, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"outcome"}),
		providerRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "provider_requests_total",
			Help:      "Model provider requests by result class; a class other than ok is an error.",
		}, []string{"class"}),
		providerRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "provider_retries_total",
			Help:      "Provider requests the retry policy scheduled again.",
		}),
		providerHeaderTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "provider_header_timeouts_total",
			Help:      "Provider calls that timed out before the response headers arrived.",
		}),
		reviewsWithoutSubmission: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "reviews_without_submission_total",
			Help:      "Review sessions that ended without submitting a review, by reason.",
		}, []string{"reason"}),
		snapshotFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "snapshot_fallbacks_total",
			Help:      "Sessions that fell back to the GitHub API tools because the snapshot was unavailable.",
		}),
		memoryFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "memory_retrieval_failures_total",
			Help:      "Pre-retrieval attempts that could not read the recorded rules.",
		}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "tokens_total",
			Help:      "Tokens a session used, by kind.",
		}, []string{"kind"}),
		estimatedCost: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "estimated_cost_usd_total",
			Help:      "Estimated provider cost of the completed sessions, in US dollars, when the provider is priced.",
		}),
	}

	m.registry.MustRegister(
		m.jobs, m.reviewDuration, m.providerRequests, m.providerRetries,
		m.providerHeaderTimeouts, m.reviewsWithoutSubmission, m.snapshotFallbacks,
		m.memoryFailures, m.tokens, m.estimatedCost,
	)
	return m
}

// Handler serves the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Job records one delivery outcome: received, queued, completed or failed.
func (m *Metrics) Job(event, outcome string) {
	if m == nil {
		return
	}
	m.jobs.WithLabelValues(event, outcome).Inc()
}

// ReviewDuration records how long a review ran and whether it submitted one.
func (m *Metrics) ReviewDuration(outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.reviewDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}

// ProviderRequest records one model step by its result class.
func (m *Metrics) ProviderRequest(class string) {
	if m == nil {
		return
	}
	m.providerRequests.WithLabelValues(class).Inc()
}

// ProviderRetry records one retry the retry policy scheduled.
func (m *Metrics) ProviderRetry() {
	if m == nil {
		return
	}
	m.providerRetries.Inc()
}

// ProviderHeaderTimeout records a provider call that never sent its headers.
func (m *Metrics) ProviderHeaderTimeout() {
	if m == nil {
		return
	}
	m.providerHeaderTimeouts.Inc()
}

// ReviewWithoutSubmission records a session that said nothing about the pull
// request, with the reason the caller can tell apart.
func (m *Metrics) ReviewWithoutSubmission(reason string) {
	if m == nil {
		return
	}
	m.reviewsWithoutSubmission.WithLabelValues(reason).Inc()
}

// SnapshotFallback records a session that read through the GitHub API because
// its working tree could not be built.
func (m *Metrics) SnapshotFallback() {
	if m == nil {
		return
	}
	m.snapshotFallbacks.Inc()
}

// MemoryFailure records a pre-retrieval that could not read the rules.
func (m *Metrics) MemoryFailure() {
	if m == nil {
		return
	}
	m.memoryFailures.Inc()
}

// Usage records the tokens a review used and, when the provider is priced, the
// estimated cost.
func (m *Metrics) Usage(input, output, cacheRead, cacheCreation int64, costUSD *float64) {
	if m == nil {
		return
	}
	for kind, value := range map[string]int64{
		"input": input, "output": output,
		"cache_read": cacheRead, "cache_creation": cacheCreation,
	} {
		if value > 0 {
			m.tokens.WithLabelValues(kind).Add(float64(value))
		}
	}
	if costUSD != nil && *costUSD > 0 {
		m.estimatedCost.Add(*costUSD)
	}
}

// Source is the read-only database view behind the queue and session gauges.
type Source interface {
	QueueState(ctx context.Context) (store.QueueState, error)
	SessionState(ctx context.Context) (store.SessionState, error)
}

// CollectDatabase registers the queue and session gauges, which are read at
// scrape time rather than maintained by the workers: a restarted process then
// reports the state of the database instead of the state it remembers.
func (m *Metrics) CollectDatabase(source Source, logger *slog.Logger) {
	if m == nil || source == nil {
		return
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m.registry.MustRegister(newDBCollector(source, logger))
}

// dbCollector reads the queue and session counts on every scrape.
type dbCollector struct {
	source Source
	logger *slog.Logger
	errors atomic.Int64

	queueDepth    *prometheus.Desc
	oldestQueued  *prometheus.Desc
	sessionStates *prometheus.Desc
	scrapeErrors  *prometheus.Desc
}

func newDBCollector(source Source, logger *slog.Logger) *dbCollector {
	return &dbCollector{
		source: source,
		logger: logger,
		queueDepth: prometheus.NewDesc(Namespace+"_queue_jobs",
			"Webhook jobs in the queue by status.", []string{"status"}, nil),
		oldestQueued: prometheus.NewDesc(Namespace+"_queue_oldest_job_age_seconds",
			"Age of the oldest queued job, zero when the queue is empty.", nil, nil),
		sessionStates: prometheus.NewDesc(Namespace+"_sessions",
			"Review sessions by status.", []string{"status"}, nil),
		scrapeErrors: prometheus.NewDesc(Namespace+"_metrics_scrape_errors_total",
			"Database reads behind /metrics that failed.", nil, nil),
	}
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queueDepth
	ch <- c.oldestQueued
	ch <- c.sessionStates
	ch <- c.scrapeErrors
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	if state, err := c.source.QueueState(ctx); err != nil {
		c.failed("reading the queue state failed", err)
	} else {
		ch <- prometheus.MustNewConstMetric(c.queueDepth, prometheus.GaugeValue, float64(state.Queued), "queued")
		ch <- prometheus.MustNewConstMetric(c.queueDepth, prometheus.GaugeValue, float64(state.Running), "running")
		ch <- prometheus.MustNewConstMetric(c.oldestQueued, prometheus.GaugeValue, state.OldestQueued.Seconds())
	}

	if state, err := c.source.SessionState(ctx); err != nil {
		c.failed("reading the session state failed", err)
	} else {
		ch <- prometheus.MustNewConstMetric(c.sessionStates, prometheus.GaugeValue, float64(state.Running), "running")
		ch <- prometheus.MustNewConstMetric(c.sessionStates, prometheus.GaugeValue, float64(state.Succeeded), "succeeded")
		ch <- prometheus.MustNewConstMetric(c.sessionStates, prometheus.GaugeValue, float64(state.Failed), "failed")
	}

	// The count is emitted from the collector rather than read from a registered
	// counter: a counter incremented during this scrape may be gathered before
	// this collector runs, which would show the previous value.
	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, float64(c.errors.Load()))
}

func (c *dbCollector) failed(message string, err error) {
	c.errors.Add(1)
	c.logger.Warn(message, "error", err)
}
