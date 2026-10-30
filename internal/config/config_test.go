package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if got := cfg.App.MaxTurns(); got != DefaultMaxAgentTurns {
		t.Errorf("max_agent_turns = %d, want %d", got, DefaultMaxAgentTurns)
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
	if got := cfg.Worker.Timeout(); got != DefaultWorkerTimeout {
		t.Errorf("worker.job_timeout = %s, want %s", got, DefaultWorkerTimeout)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("server.listen = %q, want %q", cfg.Server.Listen, DefaultListen)
	}
	if !cfg.Compaction.CompactionEnabled() {
		t.Error("compaction is off by default, want it on")
	}
	if cfg.Compaction.TriggerTokens != DefaultCompactionTriggerTokens {
		t.Errorf("compaction.trigger_tokens = %d, want %d", cfg.Compaction.TriggerTokens, DefaultCompactionTriggerTokens)
	}
	if cfg.Compaction.ReserveTokens != DefaultCompactionReserveTokens {
		t.Errorf("compaction.reserve_tokens = %d, want %d", cfg.Compaction.ReserveTokens, DefaultCompactionReserveTokens)
	}
	if cfg.Compaction.KeepRecentTokens != DefaultCompactionKeepRecentTokens {
		t.Errorf("compaction.keep_recent_tokens = %d, want %d", cfg.Compaction.KeepRecentTokens, DefaultCompactionKeepRecentTokens)
	}
	if !cfg.Snapshot.SnapshotEnabled() {
		t.Error("snapshot is off by default, want it on")
	}
	if got := cfg.Snapshot.MaxBytesOrDefault(); got != DefaultSnapshotMaxBytes {
		t.Errorf("snapshot.max_bytes = %d, want %d", got, DefaultSnapshotMaxBytes)
	}
	if got := cfg.Snapshot.TimeoutOrDefault(); got != DefaultSnapshotTimeout {
		t.Errorf("snapshot.timeout = %s, want %s", got, DefaultSnapshotTimeout)
	}
}

// The snapshot is on unless an operator turns it off, and an invalid duration
// is refused rather than silently replaced by the default.
func TestSnapshotSettings(t *testing.T) {
	doc := func(block string) []byte {
		return []byte(block + `
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

	t.Run("disabled", func(t *testing.T) {
		cfg := mustParse(t, string(doc("snapshot:\n  enabled: false\n")))
		if cfg.Snapshot.SnapshotEnabled() {
			t.Error("snapshot.enabled = false did not turn the snapshot off")
		}
	})

	t.Run("custom limits", func(t *testing.T) {
		cfg := mustParse(t, string(doc("snapshot:\n  enabled: true\n  max_bytes: 1048576\n  timeout: \"90s\"\n")))
		if got := cfg.Snapshot.MaxBytesOrDefault(); got != 1<<20 {
			t.Errorf("snapshot.max_bytes = %d, want %d", got, 1<<20)
		}
		if got := cfg.Snapshot.TimeoutOrDefault(); got != 90*time.Second {
			t.Errorf("snapshot.timeout = %s, want 90s", got)
		}
	})

	t.Run("invalid duration", func(t *testing.T) {
		if _, err := Parse(doc("snapshot:\n  timeout: \"soon\"\n"), "test.yaml"); err == nil ||
			!strings.Contains(err.Error(), "snapshot.timeout") {
			t.Errorf("Parse = %v, want it to name snapshot.timeout", err)
		}
	})
}

// An omitted cap keeps the default; an explicit zero removes it. The pointer
// is what tells those two apart.
func TestMaxAgentTurns(t *testing.T) {
	doc := func(value string) []byte {
		return []byte(value + `
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
		name      string
		app       string
		wantTurns int
		wantLimit bool
	}{
		{name: "omitted keeps the default", app: "", wantTurns: DefaultMaxAgentTurns, wantLimit: false},
		{name: "explicit zero is unlimited", app: "app:\n  max_agent_turns: 0\n", wantTurns: 0, wantLimit: true},
		{name: "a positive value is the cap", app: "app:\n  max_agent_turns: 7\n", wantTurns: 7, wantLimit: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustParse(t, string(doc(tc.app)))
			if got := cfg.App.MaxTurns(); got != tc.wantTurns {
				t.Errorf("MaxTurns() = %d, want %d", got, tc.wantTurns)
			}
			if got := cfg.App.TurnsUnlimited(); got != tc.wantLimit {
				t.Errorf("TurnsUnlimited() = %v, want %v", got, tc.wantLimit)
			}
		})
	}
}

func TestThinkingLevelValidation(t *testing.T) {
	doc := func(level string) []byte {
		return []byte(fmt.Sprintf(`
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
      thinking_level: %q
`, level))
	}

	for _, level := range ThinkingLevels {
		cfg := mustParse(t, string(doc(level)))
		if got := cfg.Models.Providers[0].ThinkingLevel; got != level {
			t.Errorf("thinking_level = %q, want %q", got, level)
		}
	}

	if _, err := Parse(doc("maximum"), "test.yaml"); err == nil {
		t.Error("Parse accepted thinking_level \"maximum\", want a validation error")
	} else if !strings.Contains(err.Error(), "thinking_level") {
		t.Errorf("error = %v, want it to name thinking_level", err)
	}
}

func TestCompactionSettings(t *testing.T) {
	base := `
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
`

	cfg := mustParse(t, base+"compaction:\n  enabled: true\n  trigger_tokens: 262144\n  reserve_tokens: 32768\n  keep_recent_tokens: 4096\n")
	if !cfg.Compaction.CompactionEnabled() {
		t.Error("CompactionEnabled() = false, want true")
	}
	if cfg.Compaction.TriggerTokens != 262144 {
		t.Errorf("trigger_tokens = %d, want 262144", cfg.Compaction.TriggerTokens)
	}
	if cfg.Compaction.ReserveTokens != 32768 {
		t.Errorf("reserve_tokens = %d, want 32768", cfg.Compaction.ReserveTokens)
	}
	if cfg.Compaction.KeepRecentTokens != 4096 {
		t.Errorf("keep_recent_tokens = %d, want 4096", cfg.Compaction.KeepRecentTokens)
	}

	cfg = mustParse(t, base+"compaction:\n  enabled: false\n")
	if cfg.Compaction.CompactionEnabled() {
		t.Error("CompactionEnabled() = true, want false when explicitly disabled")
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

// worker.job_timeout takes a Go duration string. Omitted keeps the default,
// and a malformed or non-positive value is refused at startup.
func TestWorkerJobTimeout(t *testing.T) {
	doc := func(worker string) []byte {
		return []byte(worker + `
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
		name  string
		value string
		want  time.Duration
	}{
		{name: "omitted keeps the default", value: "", want: DefaultWorkerTimeout},
		{name: "an explicit duration is used", value: "45m", want: 45 * time.Minute},
		{name: "hours are accepted", value: "2h", want: 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := ""
			if tc.value != "" {
				worker = fmt.Sprintf("worker:\n  job_timeout: %q\n", tc.value)
			}
			cfg := mustParse(t, string(doc(worker)))
			if got := cfg.Worker.Timeout(); got != tc.want {
				t.Errorf("Worker.Timeout() = %s, want %s", got, tc.want)
			}
		})
	}

	for _, value := range []string{"soon", "0s", "-5m", "15"} {
		t.Run("rejects "+value, func(t *testing.T) {
			_, err := Parse(doc(fmt.Sprintf("worker:\n  job_timeout: %q\n", value)), "test.yaml")
			if err == nil {
				t.Fatalf("Parse accepted worker.job_timeout %q", value)
			}
			if !strings.Contains(err.Error(), "worker.job_timeout") {
				t.Errorf("error = %v, want it to name worker.job_timeout", err)
			}
		})
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

// The ranking weight is a configuration value with a default, so a team that
// never mentions it gets the documented 0.05 while one that writes 0 gets a
// ranking that is the similarity alone.
func TestRepoScopeBoostDefaultsToTheDocumentedWeight(t *testing.T) {
	doc := func(memory string) []byte {
		return []byte(memory + `
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
		memory string
		want   float64
	}{
		// The literal is the weight §3 of the plan and rumil.example.yaml
		// document: the test pins the number an operator reads, not the
		// constant the code happens to carry.
		{name: "unset takes the documented weight", memory: "", want: 0.05},
		{name: "the configured value is used", memory: "memory:\n  repo_scope_boost: 0.2\n", want: 0.2},
		{name: "an explicit zero survives", memory: "memory:\n  repo_scope_boost: 0\n", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(doc(tc.memory), "test.yaml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Memory.RepoScopeWeight(); got != tc.want {
				t.Errorf("RepoScopeWeight() = %v, want %v", got, tc.want)
			}
			if DefaultRepoScopeBoost != 0.05 {
				t.Errorf("DefaultRepoScopeBoost = %v, want the documented 0.05", DefaultRepoScopeBoost)
			}
		})
	}
}

// The approval gate is per association as well as global: a team lets its own
// members record rules while strangers queue for moderation.
func TestAutoApprovesByAssociation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		moderation  Moderation
		association string
		want        bool
	}{
		{
			name:        "accept_all answers for everybody",
			moderation:  Moderation{AcceptAll: true},
			association: "NONE",
			want:        true,
		},
		{
			name:        "a listed association is approved",
			moderation:  Moderation{AutoApproveAssociations: []string{"OWNER", "MEMBER"}},
			association: "MEMBER",
			want:        true,
		},
		{
			name:        "the match ignores case and padding",
			moderation:  Moderation{AutoApproveAssociations: []string{" member "}},
			association: "MEMBER",
			want:        true,
		},
		{
			name:        "an unlisted association waits",
			moderation:  Moderation{AutoApproveAssociations: []string{"OWNER"}},
			association: "CONTRIBUTOR",
			want:        false,
		},
		{
			name:        "no list and no accept_all waits",
			moderation:  Moderation{},
			association: "OWNER",
			want:        false,
		},
		{
			name:        "an empty association waits",
			moderation:  Moderation{AutoApproveAssociations: []string{"OWNER"}},
			association: "",
			want:        false,
		},
		{
			name:        "an empty entry does not answer for a missing association",
			moderation:  Moderation{AutoApproveAssociations: []string{""}},
			association: "",
			want:        false,
		},
		{
			name:        "an empty entry does not answer for a real association",
			moderation:  Moderation{AutoApproveAssociations: []string{"  "}},
			association: "MEMBER",
			want:        false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.moderation.AutoApproves(tc.association); got != tc.want {
				t.Errorf("AutoApproves(%q) = %v, want %v", tc.association, got, tc.want)
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

// The width memory is stored and searched at is the one the embedding provider
// declares (§3 rule 5): internal/store sizes memories.embedding to it, so a
// 768-dimension model is a correct configuration, and a wide one is refused
// where pgvector's index stops.
func TestEmbeddingDimensionsComeFromTheProvider(t *testing.T) {
	doc := func(width int) []byte {
		return []byte(fmt.Sprintf(`
models:
  default_chat: "chat"
  default_embedding: "local"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
    - id: "local"
      provider: "openai-compatible"
      base_url: "http://127.0.0.1:52625/v1"
      model_name: "embed-gemma"
      api_key: "local"
      dimensions: %d
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
`, width))
	}

	cfg, err := Parse(doc(768), "test.yaml")
	if err != nil {
		t.Fatalf("Parse rejected a 768-dimension provider: %v", err)
	}
	if got := cfg.Models.EmbeddingDimensions(); got != 768 {
		t.Errorf("EmbeddingDimensions() = %d, want the provider's 768", got)
	}

	// Memory off is no width at all, which is what leaves the column alone.
	cfg.Models.DefaultEmbedding = ""
	if got := cfg.Models.EmbeddingDimensions(); got != 0 {
		t.Errorf("EmbeddingDimensions() = %d with memory off, want 0", got)
	}

	// The column has a lower bound as well: a zero width is not a width.
	if _, err := Parse(doc(0), "test.yaml"); err == nil {
		t.Error("Parse accepted a provider with no dimensions as an embedding provider")
	}
}

// One column backs memory, so every embedding provider in the file states the
// same width: a disagreement is refused where it is written rather than
// resurfacing later as a column sized to the other one.
func TestEmbeddingProvidersMustAgreeOnTheWidth(t *testing.T) {
	const doc = `
models:
  default_chat: "chat"
  default_embedding: "small"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
    - id: "small"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "small"
      api_key: "k"
      dimensions: 768
    - id: "large"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "large"
      api_key: "k"
      dimensions: 1536
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
`
	_, err := Parse([]byte(doc), "test.yaml")
	if err == nil {
		t.Fatal("Parse accepted two embedding providers of different widths")
	}
	for _, want := range []string{"disagrees", "768", "1536"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
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
			name: "a repo_scope_boost above 1",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
memory:
  repo_scope_boost: 1.5
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "memory.repo_scope_boost must be between 0 and 1",
		},
		{
			name: "a negative repo_scope_boost",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
memory:
  repo_scope_boost: -0.1
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "memory.repo_scope_boost must be between 0 and 1",
		},
		{
			name: "a repo_scope_boost that is not a number",
			doc: `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
memory:
  repo_scope_boost: .nan
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`,
			wantSub: "memory.repo_scope_boost must be between 0 and 1",
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
			name: "embedding provider wider than pgvector indexes",
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
      dimensions: 4096
`,
			wantSub: "outside 1-2000",
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
			name: "a negative compaction trigger",
			doc: `
compaction:
  trigger_tokens: -1
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
			wantSub: "compaction.trigger_tokens",
		},
		{
			name: "a keep-recent budget as large as the trigger",
			doc: `
compaction:
  trigger_tokens: 1000
  keep_recent_tokens: 1000
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
			wantSub: "compaction.keep_recent_tokens must be less than",
		},
		{
			name: "a reserve as large as the trigger",
			doc: `
compaction:
  trigger_tokens: 1000
  reserve_tokens: 1000
  keep_recent_tokens: 100
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
			wantSub: "compaction.reserve_tokens must be less than",
		},
		{
			name: "a negative max agent turns",
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

// The embeddings call has its own budget: a local backend can spend a long
// time on its first request while it loads the model, and that must not be
// conflated with the chat provider's settings.
func TestEmbeddingTimeoutSettings(t *testing.T) {
	doc := func(chat, embed string) []byte {
		return []byte(`
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  default_embedding: "embed"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
` + chat + `
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 768
` + embed)
	}

	t.Run("default", func(t *testing.T) {
		cfg := mustParse(t, string(doc("", "")))
		provider, err := cfg.Models.EmbeddingProvider()
		if err != nil {
			t.Fatalf("EmbeddingProvider: %v", err)
		}
		if got := provider.EmbeddingTimeoutOrDefault(); got != DefaultEmbeddingTimeout {
			t.Errorf("embedding timeout = %s, want %s", got, DefaultEmbeddingTimeout)
		}
	})

	t.Run("configured", func(t *testing.T) {
		cfg := mustParse(t, string(doc("", "      embedding_timeout: \"90s\"\n")))
		provider, err := cfg.Models.EmbeddingProvider()
		if err != nil {
			t.Fatalf("EmbeddingProvider: %v", err)
		}
		if got := provider.EmbeddingTimeoutOrDefault(); got != 90*time.Second {
			t.Errorf("embedding timeout = %s, want 90s", got)
		}
	})

	t.Run("invalid duration", func(t *testing.T) {
		if _, err := Parse(doc("", "      embedding_timeout: \"soon\"\n"), "test.yaml"); err == nil ||
			!strings.Contains(err.Error(), "embedding_timeout") {
			t.Errorf("Parse = %v, want it to name embedding_timeout", err)
		}
	})

	t.Run("chat provider", func(t *testing.T) {
		// The same key on the provider without dimensions is refused, because
		// there is no embeddings call for it to bound.
		if _, err := Parse(doc("      embedding_timeout: \"90s\"\n", ""), "test.yaml"); err == nil ||
			!strings.Contains(err.Error(), "embedding_timeout") {
			t.Errorf("Parse = %v, want the chat provider's embedding_timeout refused", err)
		}
	})
}

// A provider that queues a request can sit silent for minutes. The chat
// provider has its own header timeout so such an attempt fails and is retried
// instead of holding a worker for the job's whole budget.
func TestRequestTimeoutSettings(t *testing.T) {
	doc := func(chat, embed string) []byte {
		return []byte(`
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  default_embedding: "embed"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
` + chat + `
    - id: "embed"
      provider: "openai-compatible"
      base_url: "https://embed.example.com/v1"
      model_name: "embed"
      api_key: "k"
      dimensions: 768
` + embed)
	}

	t.Run("default", func(t *testing.T) {
		cfg := mustParse(t, string(doc("", "")))
		if got := cfg.Models.Providers[0].RequestTimeoutOrDefault(); got != DefaultRequestTimeout {
			t.Errorf("request timeout = %s, want %s", got, DefaultRequestTimeout)
		}
	})

	t.Run("configured", func(t *testing.T) {
		cfg := mustParse(t, string(doc("      request_timeout: \"90s\"\n", "")))
		if got := cfg.Models.Providers[0].RequestTimeoutOrDefault(); got != 90*time.Second {
			t.Errorf("request timeout = %s, want 90s", got)
		}
	})

	t.Run("invalid duration", func(t *testing.T) {
		if _, err := Parse(doc("      request_timeout: \"soon\"\n", ""), "test.yaml"); err == nil ||
			!strings.Contains(err.Error(), "request_timeout") {
			t.Errorf("Parse = %v, want it to name request_timeout", err)
		}
	})

	t.Run("embedding provider", func(t *testing.T) {
		// The key is for a chat provider; an embedding provider has
		// embedding_timeout for its calls.
		if _, err := Parse(doc("", "      request_timeout: \"90s\"\n"), "test.yaml"); err == nil ||
			!strings.Contains(err.Error(), "request_timeout") {
			t.Errorf("Parse = %v, want the embedding provider's request_timeout refused", err)
		}
	})
}
