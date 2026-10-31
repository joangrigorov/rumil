package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// watchTestInterval keeps the watcher tests fast; the production interval is
// DefaultReloadInterval.
const watchTestInterval = 5 * time.Millisecond

// waitForReload receives the next applied configuration or fails the test.
func waitForReload(t *testing.T, applied <-chan *Config) *Config {
	t.Helper()
	select {
	case cfg := <-applied:
		return cfg
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not apply the changed configuration")
		return nil
	}
}

// expectNoReload fails when the watcher applies a configuration inside d.
func expectNoReload(t *testing.T, applied <-chan *Config, d time.Duration) {
	t.Helper()
	select {
	case cfg := <-applied:
		t.Fatalf("the watcher applied an unchanged configuration (log_level %q)", cfg.App.LogLevel)
	case <-time.After(d):
	}
}

// startWatcher writes doc to path and starts a watcher whose baseline is that
// exact content. It returns the apply channel and a stop function.
func startWatcher(t *testing.T, path, doc string) (<-chan *Config, func()) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	applied := make(chan *Config, 4)
	go Watch(ctx, path, raw, watchTestInterval, slog.New(slog.DiscardHandler), func(cfg *Config) error {
		applied <- cfg
		return nil
	})
	return applied, cancel
}

func TestWatchAppliesChangesToTheActiveFileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	applied, stop := startWatcher(t, path, twoChatOneEmbedding)
	defer stop()

	// A file at another discovery location is not the active file.
	if err := os.WriteFile(filepath.Join(dir, "rumil.yaml"), []byte(twoChatOneEmbedding), 0o600); err != nil {
		t.Fatalf("writing the unrelated configuration: %v", err)
	}
	expectNoReload(t, applied, 50*time.Millisecond)

	changed := strings.Replace(twoChatOneEmbedding, `log_level: "debug"`, `log_level: "warn"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatalf("writing the changed configuration: %v", err)
	}

	cfg := waitForReload(t, applied)
	if cfg.App.LogLevel != "warn" {
		t.Errorf("log_level = %q, want warn", cfg.App.LogLevel)
	}
}

func TestWatchKeepsRunningConfigurationForAnInvalidEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	applied, stop := startWatcher(t, path, twoChatOneEmbedding)
	defer stop()

	if err := os.WriteFile(path, []byte("app: ["), 0o600); err != nil {
		t.Fatalf("writing the invalid configuration: %v", err)
	}
	expectNoReload(t, applied, 50*time.Millisecond)

	changed := strings.Replace(twoChatOneEmbedding, `log_level: "debug"`, `log_level: "error"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatalf("writing the corrected configuration: %v", err)
	}

	cfg := waitForReload(t, applied)
	if cfg.App.LogLevel != "error" {
		t.Errorf("log_level = %q, want error", cfg.App.LogLevel)
	}
}

func TestWatchRetriesARejectedDocumentAfterReverting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(twoChatOneEmbedding), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan struct{}, 8)
	go Watch(ctx, path, raw, watchTestInterval, slog.New(slog.DiscardHandler), func(cfg *Config) error {
		if cfg.App.LogLevel != "warn" {
			return nil
		}
		attempts <- struct{}{}
		return errors.New("the apply is refused for this test")
	})

	rejected := strings.Replace(twoChatOneEmbedding, `log_level: "debug"`, `log_level: "warn"`, 1)
	waitForAttempt := func() {
		t.Helper()
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatal("the watcher did not try the rejected document")
		}
	}

	if err := os.WriteFile(path, []byte(rejected), 0o600); err != nil {
		t.Fatalf("writing the rejected configuration: %v", err)
	}
	waitForAttempt()

	// Returning to the applied document clears the one-report state. The
	// rejected content is then tried again instead of being remembered as
	// already reported.
	if err := os.WriteFile(path, []byte(twoChatOneEmbedding), 0o600); err != nil {
		t.Fatalf("restoring the configuration: %v", err)
	}
	select {
	case <-attempts:
		t.Fatal("the restored document was applied instead of matching the running content")
	case <-time.After(30 * time.Millisecond):
	}

	if err := os.WriteFile(path, []byte(rejected), 0o600); err != nil {
		t.Fatalf("rewriting the rejected configuration: %v", err)
	}
	waitForAttempt()
}

func TestWatchPicksUpARecreatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	applied, stop := startWatcher(t, path, twoChatOneEmbedding)
	defer stop()

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the configuration: %v", err)
	}
	expectNoReload(t, applied, 30*time.Millisecond)

	changed := strings.Replace(twoChatOneEmbedding, `max_agent_turns: 12`, `max_agent_turns: 7`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatalf("recreating the configuration: %v", err)
	}

	cfg := waitForReload(t, applied)
	if got := cfg.App.MaxTurns(); got != 7 {
		t.Errorf("max_agent_turns = %d, want 7", got)
	}
}

func TestWatchIgnoresARewriteOfTheSameContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	applied, stop := startWatcher(t, path, twoChatOneEmbedding)
	defer stop()

	// A save that changes nothing on disk must not rebuild the engine.
	if err := os.WriteFile(path, []byte(twoChatOneEmbedding), 0o600); err != nil {
		t.Fatalf("rewriting the configuration: %v", err)
	}
	expectNoReload(t, applied, 50*time.Millisecond)
}

func TestRestartRequiredNamesTheStructuralChanges(t *testing.T) {
	current := mustParse(t, twoChatOneEmbedding)
	next := mustParse(t, `
app:
  bot_handle: "@other"
  max_agent_turns: 8
  max_diff_bytes: 4096
  log_level: "warn"

server:
  listen: ":9000"
  metrics_listen: "127.0.0.1:9090"
  public_url: "https://rumil.example.com"

database:
  dsn: "postgres://next"
  auto_migrate: true

worker:
  concurrency: 9
  job_timeout: "30m"

github:
  app_id: 999
  installation_id: 123
  private_key: "another-inline-test-key"
  webhook_secret: "next-webhook-secret"

models:
  providers:
    - id: "embed-1"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "text-embedding-3-small"
      api_key: "changed-embed-key"
      dimensions: 1536
    - id: "chat-a"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-a-v2"
      api_key: "chat-a-key"
    - id: "chat-b"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-b"
      api_key: "chat-b-key"
`)

	got := RestartRequired(current, next)
	want := []string{
		"app.bot_handle",
		"server.listen",
		"server.metrics_listen",
		"database.dsn",
		"database.auto_migrate",
		"worker.concurrency",
		"github.app_id",
		"github.private_key",
		"github.installation_id",
		"models.default_embedding",
	}
	for _, field := range want {
		if !containsField(got, field) {
			t.Errorf("RestartRequired() = %v, missing %q", got, field)
		}
	}
}

func TestApplyReloadKeepsStructuralValuesAndTakesTheRest(t *testing.T) {
	current := mustParse(t, twoChatOneEmbedding)
	next := mustParse(t, `
app:
  bot_handle: "@other"
  max_agent_turns: 8
  max_diff_bytes: 4096
  log_level: "warn"

server:
  listen: ":9000"
  metrics_listen: "127.0.0.1:9090"
  public_url: "https://rumil.example.com"

worker:
  concurrency: 9
  job_timeout: "30m"

github:
  app_id: 999
  installation_id: 123
  private_key: "another-inline-test-key"
  webhook_secret: "next-webhook-secret"

review:
  max_total_tokens: 1000

models:
  providers:
    - id: "embed-1"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "text-embedding-3-small"
      api_key: "changed-embed-key"
      dimensions: 1536
    - id: "chat-a"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-a-v2"
      api_key: "chat-a-key"
    - id: "chat-b"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-b"
      api_key: "chat-b-key"
`)

	effective, err := ApplyReload(current, next)
	if err != nil {
		t.Fatalf("ApplyReload returned error: %v", err)
	}

	if effective.App.BotHandle != current.App.BotHandle {
		t.Errorf("bot_handle = %q, want the running %q", effective.App.BotHandle, current.App.BotHandle)
	}
	if effective.Server.Listen != current.Server.Listen {
		t.Errorf("listen = %q, want the running %q", effective.Server.Listen, current.Server.Listen)
	}
	if effective.Server.MetricsListen != current.Server.MetricsListen {
		t.Errorf("metrics_listen = %q, want the running %q", effective.Server.MetricsListen, current.Server.MetricsListen)
	}
	if effective.Worker.Concurrency != current.Worker.Concurrency {
		t.Errorf("concurrency = %d, want the running %d", effective.Worker.Concurrency, current.Worker.Concurrency)
	}
	if effective.GitHub.AppID != current.GitHub.AppID {
		t.Errorf("app_id = %d, want the running %d", effective.GitHub.AppID, current.GitHub.AppID)
	}

	if effective.App.LogLevel != "warn" {
		t.Errorf("log_level = %q, want warn", effective.App.LogLevel)
	}
	if got := effective.App.MaxTurns(); got != 8 {
		t.Errorf("max_agent_turns = %d, want 8", got)
	}
	if effective.Worker.Timeout() != 30*time.Minute {
		t.Errorf("job_timeout = %s, want 30m", effective.Worker.Timeout())
	}
	if effective.Server.PublicURL != "https://rumil.example.com" {
		t.Errorf("public_url = %q, want the reloaded value", effective.Server.PublicURL)
	}
	if effective.GitHub.WebhookSecret != "next-webhook-secret" {
		t.Errorf("webhook_secret = %q, want the reloaded value", effective.GitHub.WebhookSecret)
	}
	if effective.Review.MaxTotalTokens != 1000 {
		t.Errorf("max_total_tokens = %d, want 1000", effective.Review.MaxTotalTokens)
	}

	chat, err := effective.Models.ChatProviderByID("chat-a")
	if err != nil {
		t.Fatalf("resolving the reloaded chat provider: %v", err)
	}
	if chat.ModelName != "chat-a-v2" {
		t.Errorf("chat-a model_name = %q, want chat-a-v2", chat.ModelName)
	}

	embedding, err := effective.Models.providerByID("embed-1")
	if err != nil {
		t.Fatalf("resolving the pinned embedding provider: %v", err)
	}
	if embedding.APIKey != "embed-key" {
		t.Errorf("embedding api_key = %q, want the running value", embedding.APIKey)
	}
}

func TestApplyReloadTakesPerInstallationSettings(t *testing.T) {
	withInstallations := func(name, provider string, concurrency, tokens int) string {
		doc := strings.Replace(twoChatOneEmbedding, "  webhook_secret: \"webhook-secret\"\n", `  webhook_secret: "webhook-secret"
  installations:
    - id: 111
      name: "`+name+`"
      worker_concurrency: `+strconv.Itoa(concurrency)+`
      chat_provider: "`+provider+`"
      max_total_tokens: `+strconv.Itoa(tokens)+`
`, 1)
		return doc
	}

	current := mustParse(t, withInstallations("acme", "chat-a", 2, 100))
	next := mustParse(t, withInstallations("renamed", "chat-b", 5, 500))

	effective, err := ApplyReload(current, next)
	if err != nil {
		t.Fatalf("ApplyReload returned error: %v", err)
	}
	installation := effective.GitHub.InstallationByID(111)
	if installation == nil {
		t.Fatal("the running installation is missing after the reload")
	}
	if installation.Name != "acme" {
		t.Errorf("name = %q, want the running acme", installation.Name)
	}
	if installation.WorkerConcurrency != 2 {
		t.Errorf("worker_concurrency = %d, want the running 2", installation.WorkerConcurrency)
	}
	if installation.ChatProvider != "chat-b" {
		t.Errorf("chat_provider = %q, want the reloaded chat-b", installation.ChatProvider)
	}
	if installation.MaxTotalTokens != 500 {
		t.Errorf("max_total_tokens = %d, want the reloaded 500", installation.MaxTotalTokens)
	}
	if !containsField(RestartRequired(current, next), "github.installations") {
		t.Error("RestartRequired() did not name github.installations")
	}
}

func TestApplyReloadRejectsAConflictWithPinnedSettings(t *testing.T) {
	current := mustParse(t, twoChatOneEmbedding)
	next := mustParse(t, `
app:
  bot_handle: "@rumil"
  max_agent_turns: 12
  max_diff_bytes: 4096
  log_level: "debug"

github:
  app_id: 123456
  installation_id: 98765432
  private_key: "inline-test-key"
  webhook_secret: "webhook-secret"

models:
  default_chat: "embed-1"
  providers:
    - id: "embed-1"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "now-a-chat-model"
      api_key: "embed-key"
    - id: "chat-a"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-a"
      api_key: "chat-a-key"
`)

	if _, err := ApplyReload(current, next); err == nil {
		t.Fatal("ApplyReload accepted a chat provider that is still the pinned embedding provider")
	}
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
