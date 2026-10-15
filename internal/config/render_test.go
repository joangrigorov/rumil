package config

import (
	"os"
	"strings"
	"testing"
)

// fullAnswers is a configuration that answers every question, with the secrets
// written as environment variables.
func fullAnswers() Answers {
	return Answers{
		BotHandle:        "acme-rumil",
		AppID:            "123456",
		InstallationID:   "98765432",
		PrivateKeyPath:   "GITHUB_APP_PRIVATE_KEY_PATH",
		WebhookSecret:    "0123456789abcdef",
		ChatBaseURL:      "https://api.deepseek.com/v1",
		ChatModel:        "deepseek-chat",
		ChatAPIKey:       "DEEPSEEK_API_KEY",
		EmbeddingModel:   "text-embedding-3-small",
		EmbeddingBaseURL: "https://api.openai.com/v1",
		EmbeddingAPIKey:  "OPENAI_API_KEY",
		DatabaseDSN:      "DATABASE_URL",
		AdminToken:       "RUMIL_ADMIN_TOKEN",
	}
}

// The file init writes has to be one the runtime reads, which is the only claim
// that matters about it: render it, expand the placeholders from an environment
// as an operator would have it, and load the result.
func TestRenderProducesAFileTheLoaderAccepts(t *testing.T) {
	raw, err := Render(fullAnswers())
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	environment := map[string]string{
		"GITHUB_APP_PRIVATE_KEY_PATH": "/run/secrets/github_app.pem",
		"DEEPSEEK_API_KEY":            "sk-deepseek",
		"OPENAI_API_KEY":              "sk-openai",
		"DATABASE_URL":                "postgres://rumil:rumil@db:5432/rumil?sslmode=disable",
		"RUMIL_ADMIN_TOKEN":           "a-token",
	}
	expanded := os.Expand(string(raw), func(name string) string { return environment[name] })

	cfg, err := Parse([]byte(expanded), "the rendered file")
	if err != nil {
		t.Fatalf("the rendered file does not load: %v\n%s", err, raw)
	}

	if cfg.App.BotHandle != "acme-rumil" {
		t.Errorf("bot_handle = %q, want acme-rumil", cfg.App.BotHandle)
	}
	if cfg.GitHub.AppID != 123456 || cfg.GitHub.InstallationID != 98765432 {
		t.Errorf("github ids = %d/%d, want 123456/98765432", cfg.GitHub.AppID, cfg.GitHub.InstallationID)
	}
	if cfg.GitHub.PrivateKeyPath != "/run/secrets/github_app.pem" {
		t.Errorf("private_key_path = %q, want the expanded variable", cfg.GitHub.PrivateKeyPath)
	}
	if cfg.GitHub.WebhookSecret != "0123456789abcdef" {
		t.Errorf("webhook_secret = %q", cfg.GitHub.WebhookSecret)
	}
	if cfg.Database.DSN != "postgres://rumil:rumil@db:5432/rumil?sslmode=disable" || !cfg.Database.AutoMigrate {
		t.Errorf("database = %+v, want the expanded DSN and auto_migrate on", cfg.Database)
	}
	if cfg.Moderation.AdminToken != "a-token" {
		t.Errorf("moderation.admin_token = %q", cfg.Moderation.AdminToken)
	}

	if len(cfg.Models.Providers) != 2 {
		t.Fatalf("providers = %d, want the chat and embedding providers", len(cfg.Models.Providers))
	}
	chat, embedding := cfg.Models.Providers[0], cfg.Models.Providers[1]
	if chat.ID != "deepseek-chat" || chat.APIKey != "sk-deepseek" || chat.Dimensions != 0 {
		t.Errorf("chat provider = %+v", chat)
	}
	if embedding.ID != "text-embedding-3-small" || embedding.APIKey != "sk-openai" || embedding.Dimensions != EmbeddingDimensions {
		t.Errorf("embedding provider = %+v", embedding)
	}
	if cfg.Models.DefaultChat != "deepseek-chat" || cfg.Models.DefaultEmbedding != "text-embedding-3-small" {
		t.Errorf("defaults = %q/%q", cfg.Models.DefaultChat, cfg.Models.DefaultEmbedding)
	}

	chatProvider, err := cfg.Models.ChatProvider()
	if err != nil || chatProvider == nil || chatProvider.ID != "deepseek-chat" {
		t.Errorf("the chat provider does not resolve: %v, %+v", err, chatProvider)
	}
	embeddingProvider, err := cfg.Models.EmbeddingProvider()
	if err != nil || embeddingProvider == nil || embeddingProvider.ID != "text-embedding-3-small" {
		t.Errorf("the embedding provider does not resolve: %v, %+v", err, embeddingProvider)
	}
}

// A question left empty leaves its key out, so the runtime's default applies
// and the file says only what the operator decided.
func TestRenderLeavesTheOptionalSectionsOut(t *testing.T) {
	answers := fullAnswers()
	answers.EmbeddingModel = ""
	answers.EmbeddingBaseURL = ""
	answers.EmbeddingAPIKey = ""
	answers.DatabaseDSN = ""
	answers.AdminToken = ""
	answers.WebhookSecret = ""

	raw, err := Render(answers)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	rendered := string(raw)

	for _, key := range []string{"default_embedding", "database", "auto_migrate", "admin_token", "webhook_secret", "dimensions"} {
		if strings.Contains(rendered, key) {
			t.Errorf("the file carries %s although nothing asked for it:\n%s", key, rendered)
		}
	}
	if strings.Count(rendered, "- id:") != 1 {
		t.Errorf("the file carries more than the chat provider:\n%s", rendered)
	}
}

// An embedding provider with no endpoint of its own falls back to the chat
// endpoint and the chat credential, which is what a reader would expect of
// "leave it empty".
func TestRenderFallsBackToTheChatProviderForEmbeddings(t *testing.T) {
	answers := fullAnswers()
	answers.EmbeddingBaseURL = ""
	answers.EmbeddingAPIKey = ""

	raw, err := Render(answers)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	expanded := os.Expand(string(raw), func(name string) string {
		return map[string]string{
			"GITHUB_APP_PRIVATE_KEY_PATH": "/run/secrets/github_app.pem",
			"DEEPSEEK_API_KEY":            "sk-deepseek",
			"DATABASE_URL":                "postgres://rumil:rumil@db:5432/rumil?sslmode=disable",
			"RUMIL_ADMIN_TOKEN":           "a-token",
		}[name]
	})
	cfg, err := Parse([]byte(expanded), "the rendered file")
	if err != nil {
		t.Fatalf("the rendered file does not load: %v", err)
	}

	embedding := cfg.Models.Providers[1]
	if embedding.BaseURL != "https://api.deepseek.com/v1" || embedding.APIKey != "sk-deepseek" {
		t.Errorf("embedding provider = %+v, want the chat endpoint and key", embedding)
	}
}

// A secret is written as the name of the variable that holds it, so the file
// itself can be committed, copied or backed up.
func TestRenderWritesANamedSecretAsAPlaceholder(t *testing.T) {
	raw, err := Render(fullAnswers())
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	rendered := string(raw)

	for _, placeholder := range []string{
		"${GITHUB_APP_PRIVATE_KEY_PATH}", "${DEEPSEEK_API_KEY}", "${OPENAI_API_KEY}",
		"${DATABASE_URL}", "${RUMIL_ADMIN_TOKEN}",
	} {
		if !strings.Contains(rendered, placeholder) {
			t.Errorf("the file does not carry %s:\n%s", placeholder, rendered)
		}
	}
	if strings.Contains(rendered, "sk-") {
		t.Errorf("the file carries something that looks like a key:\n%s", rendered)
	}
}

// Anything that is not the name of a variable is written as itself, because
// that is what the operator typed.
func TestRenderWritesALiteralSecretAsItself(t *testing.T) {
	answers := fullAnswers()
	answers.ChatAPIKey = "sk-literal-key"

	raw, err := Render(answers)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(raw), "sk-literal-key") {
		t.Errorf("the literal key is not in the file:\n%s", raw)
	}
	if strings.Contains(string(raw), "${sk-literal-key}") {
		t.Errorf("a literal key was wrapped as a placeholder:\n%s", raw)
	}
}

// The header points at the file that documents the keys this one leaves out.
func TestRenderStartsWithTheHeaderAndTheKeysInOrder(t *testing.T) {
	raw, err := Render(fullAnswers())
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	rendered := string(raw)

	if !strings.HasPrefix(rendered, "# Rumil configuration.") {
		t.Errorf("the file does not start with the header:\n%s", rendered)
	}
	if !strings.Contains(rendered, "rumil.example.yaml") {
		t.Error("the header does not point at rumil.example.yaml")
	}

	// The order is the order the reader expects to read them in, and yaml.v3
	// takes it from the struct.
	positions := []string{"app:", "github:", "models:", "database:", "moderation:"}
	previous := -1
	for _, key := range positions {
		at := strings.Index(rendered, "\n"+key)
		if at < 0 {
			t.Fatalf("%s is missing from the file:\n%s", key, rendered)
		}
		if at < previous {
			t.Errorf("%s comes out of order:\n%s", key, rendered)
		}
		previous = at
	}
}

func TestRenderRefusesAMissingRequiredAnswer(t *testing.T) {
	clear := map[string]func(*Answers){
		"the App's slug":      func(a *Answers) { a.BotHandle = "" },
		"the App ID":          func(a *Answers) { a.AppID = "" },
		"the installation ID": func(a *Answers) { a.InstallationID = "" },
		"the private key":     func(a *Answers) { a.PrivateKeyPath = "" },
		"the chat base URL":   func(a *Answers) { a.ChatBaseURL = "" },
		"the chat model":      func(a *Answers) { a.ChatModel = "" },
		"the chat API key":    func(a *Answers) { a.ChatAPIKey = "" },
	}

	for name, clearOne := range clear {
		t.Run(name, func(t *testing.T) {
			answers := fullAnswers()
			clearOne(&answers)

			raw, err := Render(answers)
			if err == nil {
				t.Fatalf("Render() wrote a file with no %s:\n%s", name, raw)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error = %v, want it to name %s", err, name)
			}
		})
	}
}

func TestRenderRefusesAnIDThatIsNotANumber(t *testing.T) {
	answers := fullAnswers()
	answers.AppID = "abc"

	if _, err := Render(answers); err == nil || !strings.Contains(err.Error(), "App ID") {
		t.Errorf("Render() error = %v, want one naming the App ID", err)
	}

	answers = fullAnswers()
	answers.InstallationID = "-1"
	if _, err := Render(answers); err == nil || !strings.Contains(err.Error(), "installation ID") {
		t.Errorf("Render() error = %v, want one naming the installation ID", err)
	}
}

// The loader accepts any string where an endpoint belongs and fails at the first
// review, so an endpoint that cannot work is refused here instead: the file
// init writes is one that runs.
func TestRenderRefusesAnEndpointThatIsNotAURL(t *testing.T) {
	for _, endpoint := range []string{"y", "api.openai.com/v1", "ftp://example.com/v1", "https://"} {
		answers := fullAnswers()
		answers.ChatBaseURL = endpoint

		if raw, err := Render(answers); err == nil {
			t.Errorf("Render() accepted %q as an endpoint:\n%s", endpoint, raw)
		}
	}

	answers := fullAnswers()
	answers.EmbeddingBaseURL = "not a url"
	if _, err := Render(answers); err == nil || !strings.Contains(err.Error(), "embedding base URL") {
		t.Errorf("Render() error = %v, want one naming the embedding endpoint", err)
	}
}
