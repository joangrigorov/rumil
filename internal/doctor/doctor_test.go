package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func chatConfig(baseURL, apiKey string) *config.Config {
	return &config.Config{
		Models: config.Models{
			Providers: []config.Provider{{
				ID:        "primary",
				Provider:  config.ProviderOpenAICompatible,
				BaseURL:   baseURL,
				ModelName: "test-model",
				APIKey:    apiKey,
			}},
		},
	}
}

// The server check accepts either form of admin credential: the legacy single
// token and the named list are the same authorization, and a deployment that
// rotated to named credentials is not warned about a missing token.
func TestServerCheckAcceptsNamedAdminCredentials(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Database: config.Database{DSN: "postgres://rumil@localhost/rumil"},
			GitHub:   config.GitHub{WebhookSecret: "webhook-secret-value"},
		}
	}

	t.Run("the legacy token passes", func(t *testing.T) {
		cfg := base()
		cfg.Moderation.AdminToken = "legacy-token"
		check := serverCheck(cfg, newRedactor(cfg))
		if check.Status != Pass {
			t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
		}
	})

	t.Run("a named credential passes", func(t *testing.T) {
		cfg := base()
		cfg.Moderation.AdminTokens = []config.AdminCredential{{Name: "ana", Token: "named-token"}}
		check := serverCheck(cfg, newRedactor(cfg))
		if check.Status != Pass {
			t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
		}
	})

	t.Run("no credential warns", func(t *testing.T) {
		cfg := base()
		check := serverCheck(cfg, newRedactor(cfg))
		if check.Status != Warn {
			t.Fatalf("status = %s (%s), want warn", check.Status, check.Detail)
		}
		if !strings.Contains(check.Detail, "admin credential") {
			t.Errorf("detail = %q, want it to name the missing credential", check.Detail)
		}
	})

	t.Run("a missing webhook secret fails", func(t *testing.T) {
		cfg := base()
		cfg.GitHub.WebhookSecret = ""
		check := serverCheck(cfg, newRedactor(cfg))
		if check.Status != Fail {
			t.Fatalf("status = %s (%s), want fail", check.Status, check.Detail)
		}
	})
}

// A configured secret must never reach the report, whatever text an error
// carried it in. The secrets come from the configuration, because a provider
// key has no shape doctor could recognize.
func TestRedactorRemovesEveryConfiguredSecret(t *testing.T) {
	cfg := &config.Config{
		Models: config.Models{Providers: []config.Provider{
			{APIKey: "sk-live-abcdef"},
			{APIKey: "sk-live-abcdef-extra"},
		}},
		Moderation: config.Moderation{AdminToken: "admin-token-value"},
		GitHub: config.GitHub{
			WebhookSecret: "webhook-secret-value",
			PrivateKeyPEM: "pem-secret-value",
		},
		Database: config.Database{DSN: "postgres://user:db-password@localhost:5432/rumil"},
	}

	red := newRedactor(cfg)
	got := red.text(strings.Join([]string{
		"key sk-live-abcdef-extra", "key sk-live-abcdef",
		"token admin-token-value", "hook webhook-secret-value",
		"pem pem-secret-value", "dsn postgres://user:db-password@localhost:5432/rumil",
		"password db-password",
	}, " "))

	for _, secret := range []string{
		"sk-live-abcdef-extra", "sk-live-abcdef", "admin-token-value",
		"webhook-secret-value", "pem-secret-value",
		"postgres://user:db-password@localhost:5432/rumil", "db-password",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("the report still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("the report carries no redaction marker: %s", got)
	}
}

// A value too short to be a credential is left alone: replacing it would
// mangle ordinary words and hide nothing.
func TestRedactorLeavesShortValuesAlone(t *testing.T) {
	red := newRedactor(&config.Config{Models: config.Models{Providers: []config.Provider{{APIKey: "abc"}}}})
	if got := red.text("abc is a prefix"); got != "abc is a prefix" {
		t.Errorf("redact(%q) = %q, want it unchanged", "abc is a prefix", got)
	}
}

// A warning does not fail the run; only a configured component that did not
// work does.
func TestReportFailedOnlyOnFailure(t *testing.T) {
	report := Report{Checks: []Check{
		{Name: "config", Status: Pass},
		{Name: "embedding", Status: Warn},
		{Name: "snapshot", Status: Skip},
	}}
	if report.Failed() {
		t.Error("a report with no failure says it failed")
	}

	report.Checks = append(report.Checks, Check{Name: "chat", Status: Fail})
	if !report.Failed() {
		t.Error("a report with a failure says it passed")
	}
	if counts := report.Counts(); counts[Pass] != 1 || counts[Warn] != 1 || counts[Fail] != 1 || counts[Skip] != 1 {
		t.Errorf("counts = %v, want one of each", counts)
	}
}

// The check reports latency and usage from one real HTTP round trip.
func TestChatCheckReportsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-value" {
			t.Errorf("Authorization = %q, want the configured key", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": "OK"},
			}},
			"usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1},
		})
	}))
	defer server.Close()

	cfg := chatConfig(server.URL+"/v1", "sk-test-value")
	check := chatCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Pass {
		t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
	}
	for _, want := range []string{"2 input", "1 output", "answered in"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", check.Detail, want)
		}
	}
}

// A provider that answers without usage still reviews, but the budgets cannot
// count it, so the check warns and the run stays green.
func TestChatCheckWarnsWhenUsageIsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "OK"}}},
		})
	}))
	defer server.Close()

	cfg := chatConfig(server.URL, "sk-test-value")
	check := chatCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Warn {
		t.Fatalf("status = %s (%s), want warn", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "no usage") {
		t.Errorf("detail = %q, want it to say usage was missing", check.Detail)
	}
}

// A provider's error is quoted, and a key it echoes is redacted on the way out.
func TestChatCheckRedactsTheProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "invalid key sk-test-value"},
		})
	}))
	defer server.Close()

	cfg := chatConfig(server.URL, "sk-test-value")
	check := chatCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Fail {
		t.Fatalf("status = %s, want fail", check.Status)
	}
	if strings.Contains(check.Detail, "sk-test-value") {
		t.Errorf("detail leaks the key: %s", check.Detail)
	}
	if !strings.Contains(check.Detail, "[redacted]") {
		t.Errorf("detail carries no redaction marker: %s", check.Detail)
	}
}

// The embedding check compares the vector the provider returned with the width
// the configuration declared.
func TestEmbeddingCheckUsesTheConfiguredWidth(t *testing.T) {
	vector := make([]float32, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": vector}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{Models: config.Models{
		DefaultEmbedding: "embed",
		Providers: []config.Provider{{
			ID: "embed", BaseURL: server.URL + "/v1", ModelName: "embed-model", Dimensions: 8,
		}},
	}}
	check := embeddingCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Pass {
		t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "8 dimensions") {
		t.Errorf("detail = %q, want the returned width", check.Detail)
	}
}

// A provider that returns another width is a failure: the column memory is
// sized to the declared one, so the vector could not be stored.
func TestEmbeddingCheckRefusesAnotherWidth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{1, 2, 3}}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{Models: config.Models{
		DefaultEmbedding: "embed",
		Providers: []config.Provider{{
			ID: "embed", BaseURL: server.URL, ModelName: "embed-model", Dimensions: 8,
		}},
	}}
	check := embeddingCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Fail {
		t.Fatalf("status = %s, want fail", check.Status)
	}
	if !strings.Contains(check.Detail, "3 dimensions") || !strings.Contains(check.Detail, "8") {
		t.Errorf("detail = %q, want it to name both widths", check.Detail)
	}
}

// Memory being off is supported, so the absent embedding provider is a warning.
func TestEmbeddingCheckWarnsWhenMemoryIsOff(t *testing.T) {
	cfg := chatConfig("http://example.invalid", "")
	check := embeddingCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Warn {
		t.Fatalf("status = %s, want warn", check.Status)
	}
}

// The GitHub check mints a token, reads the installation, and confirms the
// repository the operator named is visible to it.
func TestGitHubCheckReadsTheInstallationAndRepo(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	cfg := &config.Config{GitHub: config.GitHub{
		AppID:          123456,
		InstallationID: 98765432,
		PrivateKeyPEM:  string(testutil.TestPrivateKeyPEM(t)),
	}}

	check := githubCheck(context.Background(), cfg, gh.URL(), "acme/payments", newRedactor(cfg))
	if check.Status != Pass {
		t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
	}
	for _, want := range []string{"installation 98765432", "rumil", "acme/payments is visible"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", check.Detail, want)
		}
	}
}

// A key that is not a key fails the check before any token request.
func TestGitHubCheckFailsOnAWrongKey(t *testing.T) {
	cfg := &config.Config{GitHub: config.GitHub{
		AppID:          123456,
		InstallationID: 98765432,
		PrivateKeyPEM:  "this is not a pem",
	}}
	check := githubCheck(context.Background(), cfg, "", "", newRedactor(cfg))
	if check.Status != Fail {
		t.Fatalf("status = %s, want fail", check.Status)
	}
	if !strings.Contains(check.Detail, "private key") {
		t.Errorf("detail = %q, want it to name the key", check.Detail)
	}
}

// The snapshot check downloads the default branch, extracts it, and removes the
// directory it made.
func TestSnapshotCheckDownloadsAndRemovesTheTree(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	cfg := &config.Config{GitHub: config.GitHub{
		AppID:          123456,
		InstallationID: 98765432,
		PrivateKeyPEM:  string(testutil.TestPrivateKeyPEM(t)),
	}}

	check := snapshotCheck(context.Background(), cfg, gh.URL(), "acme/payments", newRedactor(cfg))
	if check.Status != Pass {
		t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
	}
	for _, want := range []string{"extracted 2 files", "acme/payments@main", "removed it"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", check.Detail, want)
		}
	}
}

// The database check reads the schema version and, with memory on, confirms
// pgvector. It opens the database without migrating it.
func TestDatabaseCheckReportsTheSchema(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_doctor_test")
	ctx := context.Background()

	db, err := store.Open(ctx, dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	db.Close()

	cfg := &config.Config{
		Database: config.Database{DSN: dsn},
		Models: config.Models{
			DefaultEmbedding: "embed",
			Providers: []config.Provider{{
				ID: "embed", ModelName: "embed-model", Dimensions: testutil.EmbeddingDimensions,
			}},
		},
	}
	check := databaseCheck(ctx, cfg, newRedactor(cfg))
	if check.Status != Pass {
		t.Fatalf("status = %s (%s), want pass", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "schema version") || !strings.Contains(check.Detail, "pgvector installed") {
		t.Errorf("detail = %q, want the version and pgvector", check.Detail)
	}
}

// A database the migrations never reached fails with its version, rather than
// being reported as reachable and healthy.
func TestDatabaseCheckFailsOnAnUnmigratedSchema(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_doctor_unmigrated_test")
	cfg := &config.Config{Database: config.Database{DSN: dsn}}

	check := databaseCheck(context.Background(), cfg, newRedactor(cfg))
	if check.Status != Fail {
		t.Fatalf("status = %s (%s), want fail", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "version 0") {
		t.Errorf("detail = %q, want the applied version", check.Detail)
	}
}

// A chat provider that cannot be reached fails the run even though every other
// optional component is absent, which is the exit-code mapping end to end.
func TestRunFailsWhenTheChatProviderIsDown(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()

	cfg := &config.Config{
		App:    config.App{BotHandle: "@rumil", LogLevel: "info"},
		GitHub: config.GitHub{AppID: 123456, InstallationID: 98765432, PrivateKeyPEM: string(testutil.TestPrivateKeyPEM(t))},
		Models: config.Models{Providers: []config.Provider{{
			ID: "primary", BaseURL: closed.URL, ModelName: "test-model",
		}}},
		Review: config.Review{GuidelinesRef: config.GuidelinesRefBase},
		Worker: config.Worker{Concurrency: 1},
	}

	report := Run(context.Background(), Options{Config: cfg, GitHubAPIBaseURL: gh.URL()})
	if !report.Failed() {
		t.Fatalf("the report does not fail although the chat provider is down: %+v", report.Checks)
	}
	if got := statusOf(report, "chat"); got != Fail {
		t.Errorf("chat status = %s, want fail", got)
	}
	if got := statusOf(report, "github"); got != Pass {
		t.Errorf("github status = %s, want pass", got)
	}
	if got := statusOf(report, "database"); got != Skip {
		t.Errorf("database status = %s, want skip", got)
	}
}

// A healthy configuration reaches every service it names and keeps the
// optional ones out of the way.
func TestRunPassesAgainstHealthyServices(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueText("OK")

	cfg := &config.Config{
		App:    config.App{BotHandle: "@rumil", LogLevel: "info"},
		GitHub: config.GitHub{AppID: 123456, InstallationID: 98765432, PrivateKeyPEM: string(testutil.TestPrivateKeyPEM(t))},
		Models: config.Models{Providers: []config.Provider{{
			ID: "primary", BaseURL: llm.URL() + "/v1", ModelName: "test-model", APIKey: "sk-doctor-secret",
		}}},
		Review: config.Review{GuidelinesRef: config.GuidelinesRefBase},
		Worker: config.Worker{Concurrency: 1},
		Server: config.Server{Listen: "127.0.0.1:8080"},
	}

	report := Run(context.Background(), Options{
		Config:           cfg,
		ConfigPath:       "/etc/rumil/rumil.yaml",
		GitHubAPIBaseURL: gh.URL(),
		Repo:             "acme/payments",
	})
	if report.Failed() {
		t.Fatalf("the healthy configuration failed: %+v", report.Checks)
	}
	for name, want := range map[string]Level{
		"config": Pass, "database": Skip, "github": Pass, "chat": Pass,
		"embedding": Warn, "snapshot": Skip, "server": Skip,
	} {
		if got := statusOf(report, name); got != want {
			t.Errorf("%s status = %s, want %s", name, got, want)
		}
	}
	if report.ConfigPath != "/etc/rumil/rumil.yaml" {
		t.Errorf("config path = %q", report.ConfigPath)
	}
	// Every secret the configuration names is absent from the whole report.
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encoding the report: %v", err)
	}
	if strings.Contains(string(encoded), "sk-doctor-secret") {
		t.Errorf("the report carries a provider key: %s", encoded)
	}
}

func statusOf(report Report, name string) Level {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}
