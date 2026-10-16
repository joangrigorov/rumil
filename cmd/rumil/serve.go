package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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
			"Reviews start only when a pull request requests the App as a reviewer.",
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

			engine, err := reviewer.NewEngine(cfg, logger, githubAPIBaseURL, db)
			if err != nil {
				return err
			}

			pool := server.NewPool(db, engine, cfg.App.BotHandle, cfg.Worker.Concurrency, logger)
			webhook := server.NewWebhook(cfg.GitHub.WebhookSecret, cfg.App.BotHandle, cfg.GitHub.InstallationID, db, logger)

			// The moderation surface is mounted with whatever the operator
			// configured: no token means every request to it is refused, and no
			// embedding provider means an edit is refused while approving the
			// rule as it was proposed still works.
			embedderClient, err := moderationEmbedder(cfg)
			if err != nil {
				return err
			}
			moderationService := moderation.NewService(db, embedderClient, cfg.GitHub.InstallationID, logger)
			moderationUI := server.NewModerationUI(moderationService, cfg.Moderation.AdminToken, logger)

			httpServer := &http.Server{
				Addr:              address,
				Handler:           server.Handler(webhook, db, moderationUI),
				ReadHeaderTimeout: readHeaderTimeout,
			}

			poolCtx, stopPool := context.WithCancel(context.Background())
			defer stopPool()
			pool.Start(poolCtx)

			serveErr := make(chan error, 1)
			go func() {
				logger.Info("listening", "address", address, "worker_concurrency", cfg.Worker.Concurrency)
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					serveErr <- err
				}
			}()

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
			stopPool()

			drained := make(chan struct{})
			go func() {
				pool.Wait()
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
