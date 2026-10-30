package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/metrics"
	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
)

// shutdownGrace is how long a shutdown waits for reviews in flight. A review
// that misses it is left RUNNING and returns to the queue on the next start.
const shutdownGrace = 30 * time.Second

// readHeaderTimeout bounds how long a client may take to send its headers.
const readHeaderTimeout = 10 * time.Second

func newServeCmd(configPath *string) *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the GitHub webhook server and the review workers",
		Long: "Run the webhook server and the review worker pool against the configured database.\n" +
			"Reviews start when a comment mentions the App with the review command, which is the\n" +
			"trigger that works where GitHub will not let an App be requested as a reviewer.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, path, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			logger := newLogger(cfg.App.LogLevel)
			logger.Info("configuration loaded", "path", path)
			warnAboutModeration(cfg, logger, true)

			if cfg.GitHub.WebhookSecret == "" {
				return fmt.Errorf("github.webhook_secret is required to run the webhook server")
			}

			address := cfg.Server.Listen
			if listen != "" {
				address = listen
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			db, err := store.Open(ctx, cfg.Database.DSN, cfg.Database.AutoMigrate, cfg.Models.EmbeddingDimensions())
			if err != nil {
				return err
			}
			defer db.Close()

			if moved, resetErr := db.ResetRunningJobs(ctx); resetErr != nil {
				return resetErr
			} else if moved > 0 {
				logger.Warn("requeued jobs interrupted by a previous run", "jobs", moved)
			}
			if interrupted, resetErr := db.InterruptRunningReviewSessions(ctx); resetErr != nil {
				return resetErr
			} else if interrupted > 0 {
				logger.Warn("marked review sessions interrupted by a previous run", "sessions", interrupted)
			}

			engine, err := reviewer.NewEngine(cfg, logger, githubAPIBaseURL, db)
			if err != nil {
				return err
			}

			// The handle decides which comments and which review requests Rumil
			// answers, so it is held against the App's own record before the
			// webhook endpoint is reachable.
			if err := checkApp(ctx, engine, cfg.App.BotHandle, logger); err != nil {
				return err
			}

			meter := metrics.New()
			meter.CollectDatabase(db, logger)

			installations := cfg.GitHub.InstallationIDs()
			// The observation deck shares the moderation credential. Its
			// recorder is attached to every pool before the workers start, so a
			// review that begins immediately is still recorded.
			sessionsUI := server.NewSessionsUI(db, cfg.Moderation.AdminCredentials(), logger,
				server.WithSessionsMetrics(meter))

			// One pool per installation: its workers claim only that account's
			// jobs, so a busy installation cannot take another's workers. An
			// installation may set its own concurrency; the rest inherit the
			// operator-wide value.
			pools := make([]*server.Pool, 0, len(installations))
			for _, id := range installations {
				concurrency := cfg.Worker.Concurrency
				if installation := cfg.GitHub.InstallationByID(id); installation != nil && installation.WorkerConcurrency > 0 {
					concurrency = installation.WorkerConcurrency
				}
				pool := server.NewPool(db, engine, cfg.App.BotHandle, concurrency,
					logger.With("installation_id", id),
					server.WithJobTimeout(cfg.Worker.Timeout()),
					server.WithMetrics(meter),
					server.WithPoolInstallations([]int64{id}))
				pool.SetSessionRecorder(sessionsUI.Recorder())
				pools = append(pools, pool)
			}

			webhook := server.NewWebhook(cfg.GitHub.WebhookSecret, cfg.App.BotHandle, cfg.GitHub.PrimaryInstallationID(), db, logger,
				server.WithWebhookMetrics(meter),
				server.WithInstallations(installations))

			// The moderation surface is mounted with whatever the operator
			// configured: no token means every request to it is refused, and no
			// embedding provider means an edit is refused while approving the
			// rule as it was proposed still works. Rules are not per-tenant, so
			// the surface records and closes findings with the primary
			// installation's token.
			embedderClient, err := moderationEmbedder(cfg)
			if err != nil {
				return err
			}
			moderationService := moderation.NewService(db, embedderClient, cfg.GitHub.PrimaryInstallationID(), logger)
			moderationUI := server.NewModerationUI(moderationService, cfg.Moderation.AdminCredentials(), logger)

			handler := server.Handler(webhook, db, moderationUI, sessionsUI)
			var metricsServer *http.Server
			if cfg.Server.MetricsListen == "" {
				// No separate listener: the endpoint shares the main one and
				// needs the admin credential like every other operator surface.
				handler = server.WithMetricsEndpoint(handler, server.MetricsHandler(meter, cfg.Moderation.AdminCredentials(), logger))
			} else {
				// A separate address is what makes it reachable: the operator
				// binds it to loopback or a firewalled interface and the
				// scraper needs no credential.
				metricsMux := http.NewServeMux()
				metricsMux.Handle("GET /metrics", meter.Handler())
				metricsServer = &http.Server{
					Addr:              cfg.Server.MetricsListen,
					Handler:           metricsMux,
					ReadHeaderTimeout: readHeaderTimeout,
				}
			}

			httpServer := &http.Server{
				Addr:              address,
				Handler:           handler,
				ReadHeaderTimeout: readHeaderTimeout,
			}

			poolCtx, stopPool := context.WithCancel(context.Background())
			defer stopPool()
			for _, pool := range pools {
				pool.Start(poolCtx)
			}

			serveErr := make(chan error, 2)
			go func() {
				logger.Info("listening", "address", address,
					"worker_concurrency", cfg.Worker.Concurrency,
					"installations", len(installations))
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					serveErr <- err
				}
			}()
			if metricsServer != nil {
				go func() {
					logger.Info("metrics listening", "address", cfg.Server.MetricsListen)
					if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						serveErr <- err
					}
				}()
			}

			select {
			case err := <-serveErr:
				return fmt.Errorf("serving HTTP: %w", err)
			case <-ctx.Done():
			}

			logger.Info("shutting down", "grace", shutdownGrace.String())

			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancelShutdown()

			// Stop accepting deliveries first, then let the reviews in flight
			// finish inside what is left of the grace period.
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				logger.Warn("closing the HTTP server failed", "error", err)
			}
			if metricsServer != nil {
				if err := metricsServer.Shutdown(shutdownCtx); err != nil {
					logger.Warn("closing the metrics server failed", "error", err)
				}
			}
			stopPool()

			drained := make(chan struct{})
			go func() {
				var wg sync.WaitGroup
				for _, pool := range pools {
					wg.Add(1)
					go func() {
						defer wg.Done()
						pool.Wait()
					}()
				}
				wg.Wait()
				close(drained)
			}()

			select {
			case <-drained:
				logger.Info("workers drained")
			case <-shutdownCtx.Done():
				logger.Warn("workers did not drain within the grace period; interrupted jobs return to the queue on the next start")
				<-drained
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "", "HTTP address to listen on (overrides server.listen)")
	return cmd
}
