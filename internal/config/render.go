package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// initHeader heads every file `rumil init` writes.
const initHeader = `# Rumil configuration.
#
# Every key this file does not set takes the default in rumil.example.yaml,
# which is in the repository and, for a package install, at
# /etc/rumil/rumil.example.yaml. ${VAR} placeholders are expanded from the
# environment before the file is parsed, which is why a secret can be written
# as the name of the variable that holds it.
`

// Placeholder is what a ${VAR} in the rendered file stands for while the file
// is validated: the operator's environment is not set up yet at that point, and
// a file that only parses once a variable is exported is still a correct file.
const Placeholder = "placeholder"

// Answers is what `rumil init` asked for. An empty optional answer leaves its
// key out of the file, so the default the runtime applies is the one
// rumil.example.yaml documents.
type Answers struct {
	BotHandle        string
	AppID            string
	InstallationID   string
	PrivateKeyPath   string
	WebhookSecret    string
	ChatBaseURL      string
	ChatModel        string
	ChatAPIKey       string
	EmbeddingBaseURL string
	EmbeddingModel   string
	EmbeddingAPIKey  string
	// EmbeddingDimensions is the width the embedding model returns. Empty
	// takes DefaultEmbeddingDimensions; memory search cannot be built without
	// a width, and the provider has to agree with it at every call.
	EmbeddingDimensions string
	DatabaseDSN         string
	AdminToken          string
}

// envName is the shape of a name a secret is written as, rather than in full:
// an upper-case identifier. `rumil init` offers OPENAI_API_KEY as the default
// for that reason, and a value that is not one of these is written literally.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Render builds the configuration file `rumil init` writes. It returns an
// error rather than a file that the runtime would refuse: the assembled
// document is parsed here, with every ${VAR} standing in as Placeholder.
func Render(a Answers) ([]byte, error) {
	for _, field := range []struct{ what, value string }{
		{"the App's slug", a.BotHandle},
		{"the App ID", a.AppID},
		{"the installation ID", a.InstallationID},
		{"the private key", a.PrivateKeyPath},
		{"the chat base URL", a.ChatBaseURL},
		{"the chat model", a.ChatModel},
		{"the chat API key", a.ChatAPIKey},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("%s is required", field.what)
		}
	}

	// The loader accepts any string where an endpoint belongs and fails much
	// later, at the first review, which is the failure `rumil init` exists to
	// prevent.
	for _, endpoint := range []struct{ what, value string }{
		{"the chat base URL", a.ChatBaseURL},
		{"the embedding base URL", a.EmbeddingBaseURL},
	} {
		if err := checkEndpoint(endpoint.what, endpoint.value); err != nil {
			return nil, err
		}
	}

	appID, err := strconv.ParseInt(strings.TrimSpace(a.AppID), 10, 64)
	if err != nil || appID <= 0 {
		return nil, fmt.Errorf("the App ID must be a positive number, got %q", a.AppID)
	}
	installationID, err := strconv.ParseInt(strings.TrimSpace(a.InstallationID), 10, 64)
	if err != nil || installationID <= 0 {
		return nil, fmt.Errorf("the installation ID must be a positive number, got %q", a.InstallationID)
	}

	chatModel := strings.TrimSpace(a.ChatModel)
	doc := document{
		App: appSection{BotHandle: a.BotHandle},
		GitHub: githubSection{
			AppID:          appID,
			InstallationID: installationID,
			PrivateKeyPath: secretOrLiteral(a.PrivateKeyPath),
			WebhookSecret:  strings.TrimSpace(a.WebhookSecret),
		},
		Models: modelsSection{DefaultChat: chatModel},
	}
	doc.Models.Providers = append(doc.Models.Providers, providerSection{
		ID:        chatModel,
		Provider:  "openai-compatible",
		BaseURL:   strings.TrimSpace(a.ChatBaseURL),
		ModelName: chatModel,
		APIKey:    secretOrLiteral(a.ChatAPIKey),
	})

	if model := strings.TrimSpace(a.EmbeddingModel); model != "" {
		// The id of a provider is the name of its model, and a provider is
		// either the chat model or the embedding model: a file that names one
		// model for both is one the loader rejects, so the answers are refused
		// here, while the file is still the only thing that would have been
		// written.
		if model == chatModel {
			return nil, fmt.Errorf("the embedding model is the chat model, %q: a provider is either the chat model or the embedding model, so the embedding answers need an embeddings model of their own, or an empty model to run without memory", model)
		}

		baseURL := strings.TrimSpace(a.EmbeddingBaseURL)
		if baseURL == "" {
			baseURL = strings.TrimSpace(a.ChatBaseURL)
		}
		apiKey := strings.TrimSpace(a.EmbeddingAPIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(a.ChatAPIKey)
		}

		// The width is the embedding model's, not the chat model's, and the
		// column memory searches is built to it.
		width := DefaultEmbeddingDimensions
		if given := strings.TrimSpace(a.EmbeddingDimensions); given != "" {
			parsed, err := strconv.Atoi(given)
			if err != nil || parsed < 1 || parsed > MaxEmbeddingDimensions {
				return nil, fmt.Errorf("the embedding dimensions must be a whole number between 1 and %d, got %q",
					MaxEmbeddingDimensions, given)
			}
			width = parsed
		}

		doc.Models.DefaultEmbedding = model
		doc.Models.Providers = append(doc.Models.Providers, providerSection{
			ID:         model,
			Provider:   "openai-compatible",
			BaseURL:    baseURL,
			ModelName:  model,
			APIKey:     secretOrLiteral(apiKey),
			Dimensions: width,
		})
	}

	if dsn := strings.TrimSpace(a.DatabaseDSN); dsn != "" {
		doc.Database = &databaseSection{DSN: secretOrLiteral(dsn), AutoMigrate: true}
	}
	if token := strings.TrimSpace(a.AdminToken); token != "" {
		doc.Moderation = &moderationSection{AdminToken: secretOrLiteral(token)}
	}

	// Two-space indentation, because that is what rumil.example.yaml uses and
	// the two files are read side by side.
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("rendering the configuration: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("rendering the configuration: %w", err)
	}
	raw := append([]byte(initHeader), buf.Bytes()...)

	// The environment this file is written for is not set up yet, so the
	// validation sees a stand-in for every placeholder. What it still catches
	// is everything that does not depend on the environment: a key that is not
	// part of the schema, a value the loader refuses, a required value missing.
	if _, err := Parse([]byte(os.Expand(string(raw), func(string) string { return Placeholder })), "the configuration being written"); err != nil {
		return nil, err
	}
	return raw, nil
}

// secretOrLiteral writes a value as the name of the environment variable that
// holds it when it is an upper-case identifier, and as itself otherwise.
func secretOrLiteral(value string) string {
	value = strings.TrimSpace(value)
	if IsEnvName(value) {
		return "${" + value + "}"
	}
	return value
}

// IsEnvName reports whether a value is written into the file as the name of the
// environment variable that holds it rather than as itself. `rumil init` asks
// the same question of an answer, so that what it promises is what it writes.
func IsEnvName(value string) bool {
	return envName.MatchString(strings.TrimSpace(value))
}

// checkEndpoint refuses an endpoint that is not an http or https URL.
func checkEndpoint(what, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s is a URL like https://api.openai.com/v1, got %q", what, value)
	}
	return nil
}

type document struct {
	App        appSection         `yaml:"app"`
	GitHub     githubSection      `yaml:"github"`
	Models     modelsSection      `yaml:"models"`
	Database   *databaseSection   `yaml:"database,omitempty"`
	Moderation *moderationSection `yaml:"moderation,omitempty"`
}

type appSection struct {
	BotHandle string `yaml:"bot_handle"`
}

type githubSection struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	WebhookSecret  string `yaml:"webhook_secret,omitempty"`
}

type providerSection struct {
	ID         string `yaml:"id"`
	Provider   string `yaml:"provider"`
	BaseURL    string `yaml:"base_url"`
	ModelName  string `yaml:"model_name"`
	APIKey     string `yaml:"api_key"`
	Dimensions int    `yaml:"dimensions,omitempty"`
}

type modelsSection struct {
	DefaultChat      string            `yaml:"default_chat"`
	DefaultEmbedding string            `yaml:"default_embedding,omitempty"`
	Providers        []providerSection `yaml:"providers"`
}

type databaseSection struct {
	DSN         string `yaml:"dsn"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

type moderationSection struct {
	AdminToken string `yaml:"admin_token"`
}
