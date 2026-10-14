package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoChatOneEmbedding lists the embedding provider first on purpose: the
// selection rule must skip it when default_chat is omitted.
const twoChatOneEmbedding = `
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
  providers:
    - id: "embed-1"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "text-embedding-3-small"
      api_key: "embed-key"
      dimensions: 1536
    - id: "chat-a"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-a"
      api_key: "chat-a-key"
      temperature: 0.2
      max_tokens: 4096
    - id: "chat-b"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat-b"
      api_key: "chat-b-key"
`

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(doc), "test.yaml")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	return cfg
}

func TestDefaultsAreApplied(t *testing.T) {
	cfg := mustParse(t, `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`)
	if cfg.App.BotHandle != DefaultBotHandle {
		t.Errorf("bot_handle = %q, want %q", cfg.App.BotHandle, DefaultBotHandle)
	}
	if cfg.App.MaxAgentTurns != DefaultMaxAgentTurns {
		t.Errorf("max_agent_turns = %d, want %d", cfg.App.MaxAgentTurns, DefaultMaxAgentTurns)
	}
	if cfg.App.MaxDiffBytes != DefaultMaxDiffBytes {
		t.Errorf("max_diff_bytes = %d, want %d", cfg.App.MaxDiffBytes, DefaultMaxDiffBytes)
	}
	if cfg.App.LogLevel != DefaultLogLevel {
		t.Errorf("log_level = %q, want %q", cfg.App.LogLevel, DefaultLogLevel)
	}
	if cfg.Review.GuidelinesRef != DefaultGuidelinesRef {
		t.Errorf("review.guidelines_ref = %q, want %q", cfg.Review.GuidelinesRef, DefaultGuidelinesRef)
	}
	if cfg.Worker.Concurrency != DefaultWorkerConcurrency {
		t.Errorf("worker.concurrency = %d, want %d", cfg.Worker.Concurrency, DefaultWorkerConcurrency)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("server.listen = %q, want %q", cfg.Server.Listen, DefaultListen)
	}
}

// worker.concurrency must be positive when it is set at all.
func TestWorkerConcurrencyValidation(t *testing.T) {
	doc := []byte(`
worker:
  concurrency: 0
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`)

	cfg, err := Parse(doc, "test.yaml")
	if err != nil {
		t.Fatalf("Parse rejected an omitted concurrency: %v", err)
	}
	if cfg.Worker.Concurrency != DefaultWorkerConcurrency {
		t.Errorf("concurrency = %d, want the default for an omitted value", cfg.Worker.Concurrency)
	}

	if _, err := Parse([]byte(strings.Replace(string(doc), "concurrency: 0", "concurrency: -1", 1)), "test.yaml"); err == nil {
		t.Error("Parse accepted worker.concurrency -1, want a validation error")
	}
}

// review.guidelines_ref accepts the two named sources and rejects anything else.
func TestResolveThreadsDefaultsOn(t *testing.T) {
	doc := func(review string) []byte {
		return []byte(review + `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`)
	}

	for _, tc := range []struct {
		name   string
		review string
		wantOn bool
	}{
		{name: "unset resolves as on", review: "review:\n  guidelines_ref: \"base\"\n", wantOn: true},
		{name: "explicit true", review: "review:\n  guidelines_ref: \"base\"\n  resolve_threads: true\n", wantOn: true},
		{name: "explicit false survives", review: "review:\n  guidelines_ref: \"base\"\n  resolve_threads: false\n", wantOn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(doc(tc.review), "test.yaml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Review.ThreadsResolvable(); got != tc.wantOn {
				t.Errorf("ThreadsResolvable() = %v, want %v", got, tc.wantOn)
			}
		})
	}
}

func TestEmbeddingProviderIsOptIn(t *testing.T) {
	doc := func(defaultEmbedding string) []byte {
		return []byte(`
models:
  default_chat: "chat"
  default_embedding: "` + defaultEmbedding + `"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 1536
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
`)
	}

	cfg, err := Parse(doc(""), "test.yaml")
	if err != nil {
		t.Fatalf("Parse rejected an unset default_embedding: %v", err)
	}
	provider, err := cfg.Models.EmbeddingProvider()
	if err != nil {
		t.Fatalf("EmbeddingProvider: %v", err)
	}
	if provider != nil {
		t.Errorf("EmbeddingProvider = %q, want nil when default_embedding is unset", provider.ID)
	}

	cfg, err = Parse(doc("embed"), "test.yaml")
	if err != nil {
		t.Fatalf("Parse rejected an embedding provider: %v", err)
	}
	provider, err = cfg.Models.EmbeddingProvider()
	if err != nil {
		t.Fatalf("EmbeddingProvider: %v", err)
	}
	if provider == nil || provider.ID != "embed" {
		t.Errorf("EmbeddingProvider = %v, want the provider named by default_embedding", provider)
	}

	if _, err := Parse(doc("chat"), "test.yaml"); err == nil {
		t.Error("Parse accepted a default_embedding that names a chat provider")
	}
}

func TestReviewGuidelinesRef(t *testing.T) {
	doc := func(ref string) []byte {
		return []byte(`
review:
  guidelines_ref: "` + ref + `"
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`)
	}

	for _, ref := range []string{GuidelinesRefBase, GuidelinesRefHead} {
		cfg, err := Parse(doc(ref), "test.yaml")
		if err != nil {
			t.Fatalf("Parse rejected guidelines_ref %q: %v", ref, err)
		}
		if cfg.Review.GuidelinesRef != ref {
			t.Errorf("guidelines_ref = %q, want %q", cfg.Review.GuidelinesRef, ref)
		}
	}

	if _, err := Parse(doc("branch"), "test.yaml"); err == nil {
		t.Error("Parse accepted guidelines_ref \"branch\", want a validation error")
	} else if !strings.Contains(err.Error(), "review.guidelines_ref") {
		t.Errorf("error %q does not name review.guidelines_ref", err)
	}
}

// T-001-01 (configuration half): an explicit default_chat selects that provider
// even when it is not the first entry.
func TestT001_01_DefaultChatSelectsSecondProvider(t *testing.T) {
	cfg := mustParse(t, strings.Replace(twoChatOneEmbedding, "models:\n  providers:", "models:\n  default_chat: \"chat-b\"\n  providers:", 1))

	got, err := cfg.Models.ChatProvider()
	if err != nil {
		t.Fatalf("ChatProvider returned error: %v", err)
	}
	if got.ID != "chat-b" {
		t.Errorf("selected provider = %q, want \"chat-b\"", got.ID)
	}
	if got.ModelName != "chat-b" || got.APIKey != "chat-b-key" {
		t.Errorf("selected provider = %+v, want the chat-b entry", got)
	}
}

// T-001-02 (configuration half): with default_chat omitted, the first
// non-embedding provider wins, even when an embedding provider is listed first.
func TestT001_02_FirstChatProviderIsSelectedWhenDefaultOmitted(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding)

	got, err := cfg.Models.ChatProvider()
	if err != nil {
		t.Fatalf("ChatProvider returned error: %v", err)
	}
	if got.ID != "chat-a" {
		t.Errorf("selected provider = %q, want the first non-embedding provider \"chat-a\"", got.ID)
	}
}

func TestEnvironmentPlaceholdersAreExpanded(t *testing.T) {
	t.Setenv("RUMIL_TEST_CHAT_KEY", "expanded-key")
	t.Setenv("RUMIL_TEST_BASE_URL", "https://expanded.example.com/v1")

	cfg := mustParse(t, `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "${RUMIL_TEST_BASE_URL}"
      model_name: "chat"
      api_key: "${RUMIL_TEST_CHAT_KEY}"
`)
	if got := cfg.Models.Providers[0].APIKey; got != "expanded-key" {
		t.Errorf("api_key = %q, want \"expanded-key\"", got)
	}
	if got := cfg.Models.Providers[0].BaseURL; got != "https://expanded.example.com/v1" {
		t.Errorf("base_url = %q, want the expanded value", got)
	}
}

func TestParseRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr error
		wantSub string
	}{
		{
			name: "no providers",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers: []
`,
			wantErr: ErrMissingProviders,
		},
		{
			name: "unknown default_chat",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  default_chat: "nope"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantErr: ErrUnknownProvider,
		},
		{
			name: "default_chat points at an embedding provider",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  default_chat: "embed"
  providers:
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 1536
`,
			wantSub: "embedding provider",
		},
		{
			name: "only embedding providers and no default_chat",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 1536
`,
			wantErr: ErrNoChatProvider,
		},
		{
			name: "embedding dimensions mismatch",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 768
`,
			wantSub: "vector(1536)",
		},
		{
			name: "duplicate provider id",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "duplicated",
		},
		{
			name: "unsupported provider kind",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "anthropic"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "not supported",
		},
		{
			name: "missing api key",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
`,
			wantSub: "api_key is required",
		},
		{
			name: "unknown yaml field",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
  unexpected_field: true
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "unexpected_field",
		},
		{
			name: "invalid log level",
			doc: `
app:
  log_level: "verbose"
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "log_level",
		},
		{
			name: "missing app id",
			doc: `
github:
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "github.app_id",
		},
		{
			name: "missing installation id",
			doc: `
github:
  app_id: 1
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "github.installation_id",
		},
		{
			name: "missing private key",
			doc: `
github:
  app_id: 1
  installation_id: 2
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "private_key",
		},
		{
			name: "zero max agent turns",
			doc: `
app:
  max_agent_turns: -1
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "max_agent_turns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc), "test.yaml")
			if err == nil {
				t.Fatalf("Parse succeeded, want error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want to wrap %v", err, tt.wantErr)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestPrivateKeyResolution(t *testing.T) {
	t.Run("path wins over inline and environment", func(t *testing.T) {
		t.Setenv("GITHUB_PRIVATE_KEY", "env-key")
		dir := t.TempDir()
		path := filepath.Join(dir, "app.pem")
		if err := os.WriteFile(path, []byte("path-key\n"), 0o600); err != nil {
			t.Fatalf("writing key file: %v", err)
		}

		got, err := GitHub{PrivateKeyPath: path, PrivateKeyPEM: "inline-key"}.ResolvePrivateKey()
		if err != nil {
			t.Fatalf("PrivateKey returned error: %v", err)
		}
		if strings.TrimSpace(string(got)) != "path-key" {
			t.Errorf("key = %q, want the file contents", got)
		}
	})

	t.Run("inline wins over environment", func(t *testing.T) {
		t.Setenv("GITHUB_PRIVATE_KEY", "env-key")

		got, err := GitHub{PrivateKeyPEM: "inline-key"}.ResolvePrivateKey()
		if err != nil {
			t.Fatalf("PrivateKey returned error: %v", err)
		}
		if string(got) != "inline-key" {
			t.Errorf("key = %q, want \"inline-key\"", got)
		}
	})

	t.Run("environment is the last resort", func(t *testing.T) {
		t.Setenv("GITHUB_PRIVATE_KEY", "env-key")

		got, err := GitHub{}.ResolvePrivateKey()
		if err != nil {
			t.Fatalf("PrivateKey returned error: %v", err)
		}
		if string(got) != "env-key" {
			t.Errorf("key = %q, want \"env-key\"", got)
		}
	})

	t.Run("empty key file is rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.pem")
		if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
			t.Fatalf("writing key file: %v", err)
		}

		if _, err := (GitHub{PrivateKeyPath: path}).ResolvePrivateKey(); err == nil {
			t.Fatal("PrivateKey succeeded for an empty file, want error")
		}
	})

	t.Run("missing everything is rejected", func(t *testing.T) {
		if _, err := (GitHub{}).ResolvePrivateKey(); err == nil {
			t.Fatal("PrivateKey succeeded with no key configured, want error")
		}
	})
}
