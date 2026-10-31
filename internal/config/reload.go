package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"time"
)

// DefaultReloadInterval is how often `rumil serve` checks its active
// configuration file for changes. A reload applies after two consecutive reads
// agree, so a save that takes several writes is not parsed half-written.
const DefaultReloadInterval = 2 * time.Second

// Watch polls path and calls apply with each changed document that parses and
// validates. initial is the document the running process loaded, so an
// unchanged file is not applied again.
//
// Only path is polled. Discovery is not repeated: a configuration file that
// appears at another candidate location is ignored for the life of the
// process. A file that becomes unreadable keeps the running configuration and
// is picked up again when it can be read.
func Watch(ctx context.Context, path string, initial []byte, interval time.Duration,
	logger *slog.Logger, apply func(*Config) error) {
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	applied := initial
	var pending, attempted []byte
	unreadable := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			pending = nil
			if !unreadable {
				logger.Warn("the active configuration file cannot be read; keeping the running configuration",
					"path", path, "error", err)
				unreadable = true
			}
			continue
		}
		if unreadable {
			logger.Info("the active configuration file is readable again", "path", path)
			unreadable = false
		}

		if bytes.Equal(raw, applied) {
			pending = nil
			attempted = nil
			continue
		}
		// Wait for a second read that agrees before parsing, so a multi-write
		// save is not mistaken for a complete document.
		if !bytes.Equal(raw, pending) {
			pending = raw
			continue
		}
		// The same invalid document is reported once. A later edit changes the
		// bytes and is tried again.
		if bytes.Equal(raw, attempted) {
			pending = nil
			continue
		}
		attempted = raw

		cfg, err := Parse(raw, path)
		if err != nil {
			logger.Warn("the changed configuration is invalid; keeping the running configuration",
				"path", path, "error", err)
			pending = nil
			continue
		}
		if err := apply(cfg); err != nil {
			logger.Warn("the changed configuration cannot be applied; keeping the running configuration",
				"path", path, "error", err)
			pending = nil
			continue
		}

		applied = raw
		pending = nil
		logger.Info("configuration reloaded", "path", path)
	}
}

// RestartRequired lists the settings next changes that the running process
// cannot adopt. The caller logs them and keeps the current values.
func RestartRequired(current, next *Config) []string {
	var fields []string

	if current.App.BotHandle != next.App.BotHandle {
		fields = append(fields, "app.bot_handle")
	}
	if current.Server.Listen != next.Server.Listen {
		fields = append(fields, "server.listen")
	}
	if current.Server.MetricsListen != next.Server.MetricsListen {
		fields = append(fields, "server.metrics_listen")
	}
	if current.Database.DSN != next.Database.DSN {
		fields = append(fields, "database.dsn")
	}
	if current.Database.AutoMigrate != next.Database.AutoMigrate {
		fields = append(fields, "database.auto_migrate")
	}
	if current.Worker.Concurrency != next.Worker.Concurrency {
		fields = append(fields, "worker.concurrency")
	}
	if current.GitHub.AppID != next.GitHub.AppID {
		fields = append(fields, "github.app_id")
	}
	if current.GitHub.PrivateKeyPath != next.GitHub.PrivateKeyPath {
		fields = append(fields, "github.private_key_path")
	}
	if current.GitHub.PrivateKeyPEM != next.GitHub.PrivateKeyPEM {
		fields = append(fields, "github.private_key")
	}
	if current.GitHub.InstallationID != next.GitHub.InstallationID {
		fields = append(fields, "github.installation_id")
	}
	if !sameInstallations(current.GitHub.Installations, next.GitHub.Installations) {
		fields = append(fields, "github.installations")
	}
	if embeddingChanged(current.Models, next.Models) {
		fields = append(fields, "models.default_embedding")
	}

	return fields
}

// ApplyReload returns the configuration the running process should adopt after
// loading next. Reloadable settings come from next; settings that need a
// restart keep their current value. The result is validated against the merged
// document, because a reloadable change can conflict with a pinned one.
func ApplyReload(current, next *Config) (*Config, error) {
	effective := *next

	effective.App.BotHandle = current.App.BotHandle
	effective.Server.Listen = current.Server.Listen
	effective.Server.MetricsListen = current.Server.MetricsListen
	effective.Database = current.Database
	effective.Worker.Concurrency = current.Worker.Concurrency
	effective.GitHub.AppID = current.GitHub.AppID
	effective.GitHub.PrivateKeyPath = current.GitHub.PrivateKeyPath
	effective.GitHub.PrivateKeyPEM = current.GitHub.PrivateKeyPEM
	effective.GitHub.InstallationID = current.GitHub.InstallationID
	effective.GitHub.Installations = mergeInstallations(current.GitHub.Installations, next.GitHub.Installations)
	effective.Models = mergeModels(current.Models, next.Models)

	if err := effective.Validate(); err != nil {
		return nil, fmt.Errorf("the reloaded configuration conflicts with a setting that needs a restart: %w", err)
	}
	return &effective, nil
}

// sameInstallations compares the structural fields of the installation list.
// An installation's provider and budgets are read per review, so they may
// change on reload; its id, name, worker count and position may not.
func sameInstallations(current, next []Installation) bool {
	if len(current) != len(next) {
		return false
	}
	for i := range current {
		if current[i].ID != next[i].ID ||
			current[i].Name != next[i].Name ||
			current[i].WorkerConcurrency != next[i].WorkerConcurrency {
			return false
		}
	}
	return true
}

// mergeInstallations keeps the running process's installation list and takes
// the per-review settings from next for the installations it already serves.
// An installation added on disk starts working after a restart.
func mergeInstallations(current, next []Installation) []Installation {
	byID := make(map[int64]Installation, len(next))
	for _, installation := range next {
		byID[installation.ID] = installation
	}

	merged := make([]Installation, 0, len(current))
	for _, installation := range current {
		if update, ok := byID[installation.ID]; ok {
			installation.ChatProvider = update.ChatProvider
			installation.MaxTotalTokens = update.MaxTotalTokens
			installation.MaxCostUSD = update.MaxCostUSD
		}
		merged = append(merged, installation)
	}
	return merged
}

// embeddingChanged reports whether the embedding half of the model registry
// changed. The database column that stores vectors is sized from it at
// startup, so it is restart-only.
func embeddingChanged(current, next Models) bool {
	if current.DefaultEmbedding != next.DefaultEmbedding {
		return true
	}
	return !reflect.DeepEqual(embeddingProviders(current), embeddingProviders(next))
}

// embeddingProviders indexes the providers that produce vectors by id.
func embeddingProviders(models Models) map[string]Provider {
	providers := make(map[string]Provider)
	for _, provider := range models.Providers {
		if provider.IsEmbedding() {
			providers[provider.ID] = provider
		}
	}
	return providers
}

// mergeModels takes the chat half of the registry from next and the embedding
// half from current. The selectors follow the same split.
func mergeModels(current, next Models) Models {
	merged := next
	merged.DefaultEmbedding = current.DefaultEmbedding

	pinned := make(map[string]bool)
	for _, provider := range current.Providers {
		if provider.IsEmbedding() {
			pinned[provider.ID] = true
		}
	}

	providers := make([]Provider, 0, len(next.Providers)+len(pinned))
	for _, provider := range next.Providers {
		// An id that the running process uses for embeddings keeps that role
		// until a restart, even if next defines it as a chat provider.
		if provider.IsEmbedding() || pinned[provider.ID] {
			continue
		}
		providers = append(providers, provider)
	}
	for _, provider := range current.Providers {
		if provider.IsEmbedding() {
			providers = append(providers, provider)
		}
	}

	merged.Providers = providers
	return merged
}
