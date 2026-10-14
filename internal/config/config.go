// Package config loads, defaults, and validates the Rumil configuration file.
//
// The schema is the one specified in project_plan.md §3. Loading expands
// ${ENV_VAR} placeholders before the YAML is parsed, so secrets never need to
// live in the file itself.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// EmbeddingDimensions is the fixed width of the memories.embedding column
// (project_plan.md §4). A configured embedding provider must match it.
const EmbeddingDimensions = 1536

// ProviderOpenAICompatible is the only provider kind supported in v0.0.1.
const ProviderOpenAICompatible = "openai-compatible"

// Defaults applied to fields the configuration file omits.
const (
	DefaultBotHandle     = "@rumil"
	DefaultMaxAgentTurns = 25
	DefaultMaxDiffBytes  = 250_000
	DefaultLogLevel      = "info"
	// DefaultGuidelinesRef reads RUMIL.md from the pull request base SHA, so
	// an untrusted branch cannot rewrite the repository's review policy.
	DefaultGuidelinesRef = "base"
)

// GuidelinesRef values accepted by review.guidelines_ref.
const (
	GuidelinesRefBase = "base"
	GuidelinesRefHead = "head"
)

// Errors returned by Validate, wrapped with field context.
var (
	ErrMissingProviders = errors.New("models.providers must contain at least one provider")
	ErrUnknownProvider  = errors.New("models.default_chat does not match any models.providers id")
	ErrNoChatProvider   = errors.New("models.providers contains no chat provider")
)

// Config is the whole rumil.yaml document.
type Config struct {
	App        App        `yaml:"app"`
	GitHub     GitHub     `yaml:"github"`
	Review     Review     `yaml:"review"`
	Models     Models     `yaml:"models"`
	Database   Database   `yaml:"database"`
	Moderation Moderation `yaml:"moderation"`
}

// App holds process-wide behavior switches.
type App struct {
	BotHandle     string `yaml:"bot_handle"`
	MaxAgentTurns int    `yaml:"max_agent_turns"`
	MaxDiffBytes  int    `yaml:"max_diff_bytes"`
	LogLevel      string `yaml:"log_level"`
}

// GitHub holds the operator's own GitHub App credentials. There is no
// centralized Rumil App: app_id, installation_id, and a private key are all
// required (project_plan.md §3).
type GitHub struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	PrivateKeyPEM  string `yaml:"private_key"`
	WebhookSecret  string `yaml:"webhook_secret"`
}

// Models is the BYOK model registry plus the default selectors.
type Models struct {
	DefaultChat      string     `yaml:"default_chat"`
	DefaultEmbedding string     `yaml:"default_embedding"`
	Providers        []Provider `yaml:"providers"`
}

// Provider is one OpenAI-compatible endpoint. A provider with dimensions set
// is an embedding provider; every other provider is a chat provider.
type Provider struct {
	ID          string   `yaml:"id"`
	Provider    string   `yaml:"provider"`
	BaseURL     string   `yaml:"base_url"`
	ModelName   string   `yaml:"model_name"`
	APIKey      string   `yaml:"api_key"`
	Temperature *float64 `yaml:"temperature"`
	MaxTokens   int      `yaml:"max_tokens"`
	Dimensions  int      `yaml:"dimensions"`
}

// IsEmbedding reports whether the provider produces embeddings.
func (p Provider) IsEmbedding() bool { return p.Dimensions > 0 }

// Review tunes the repository guidelines injected into the review prompt.
type Review struct {
	GuidelinesRef string `yaml:"guidelines_ref"`
}

// Database is used from v0.1.0 on.
type Database struct {
	DSN         string `yaml:"dsn"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// Moderation is used from v0.2.0 on.
type Moderation struct {
	AcceptAll               bool     `yaml:"accept_all"`
	AutoApproveAssociations []string `yaml:"auto_approve_associations"`
	AdminToken              string   `yaml:"admin_token"`
}

// Load reads path, expands ${ENV_VAR} placeholders, parses the YAML, applies
// defaults, and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	return Parse(raw, path)
}

// Parse behaves like Load for an in-memory document. source names the origin
// in error messages.
func Parse(raw []byte, source string) (*Config, error) {
	expanded := os.Expand(string(raw), os.Getenv)

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", source, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", source, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.App.BotHandle == "" {
		c.App.BotHandle = DefaultBotHandle
	}
	if c.App.MaxAgentTurns == 0 {
		c.App.MaxAgentTurns = DefaultMaxAgentTurns
	}
	if c.App.MaxDiffBytes == 0 {
		c.App.MaxDiffBytes = DefaultMaxDiffBytes
	}
	if c.App.LogLevel == "" {
		c.App.LogLevel = DefaultLogLevel
	}
	if c.Review.GuidelinesRef == "" {
		c.Review.GuidelinesRef = DefaultGuidelinesRef
	}
}

// Validate enforces the schema rules that v0.0.1 depends on: app limits, GitHub
// App credential presence, and the model selection rules.
func (c *Config) Validate() error {
	if c.App.MaxAgentTurns < 1 {
		return fmt.Errorf("app.max_agent_turns must be at least 1, got %d", c.App.MaxAgentTurns)
	}
	if c.App.MaxDiffBytes < 1 {
		return fmt.Errorf("app.max_diff_bytes must be at least 1, got %d", c.App.MaxDiffBytes)
	}
	switch c.App.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("app.log_level must be one of debug, info, warn, error, got %q", c.App.LogLevel)
	}

	switch c.Review.GuidelinesRef {
	case GuidelinesRefBase, GuidelinesRefHead:
	default:
		return fmt.Errorf("review.guidelines_ref must be %q or %q, got %q", GuidelinesRefBase, GuidelinesRefHead, c.Review.GuidelinesRef)
	}

	if err := c.GitHub.validate(); err != nil {
		return err
	}
	return c.Models.validate()
}

func (g GitHub) validate() error {
	if g.AppID <= 0 {
		return fmt.Errorf("github.app_id must be a positive integer, got %d", g.AppID)
	}
	if g.InstallationID <= 0 {
		return fmt.Errorf("github.installation_id must be a positive integer, got %d", g.InstallationID)
	}
	if g.PrivateKeyPath == "" && g.PrivateKeyPEM == "" && os.Getenv("GITHUB_PRIVATE_KEY") == "" {
		return errors.New("github.private_key_path, github.private_key, or the GITHUB_PRIVATE_KEY environment variable is required")
	}
	return nil
}

func (m Models) validate() error {
	if len(m.Providers) == 0 {
		return ErrMissingProviders
	}

	seen := make(map[string]struct{}, len(m.Providers))
	for i, p := range m.Providers {
		if p.ID == "" {
			return fmt.Errorf("models.providers[%d].id is required", i)
		}
		if _, dup := seen[p.ID]; dup {
			return fmt.Errorf("models.providers[%d].id %q is duplicated", i, p.ID)
		}
		seen[p.ID] = struct{}{}

		switch p.Provider {
		case ProviderOpenAICompatible:
		default:
			return fmt.Errorf("models.providers[%d].provider %q is not supported, use %q", i, p.Provider, ProviderOpenAICompatible)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("models.providers[%d].base_url is required", i)
		}
		if p.ModelName == "" {
			return fmt.Errorf("models.providers[%d].model_name is required", i)
		}
		if p.APIKey == "" {
			return fmt.Errorf("models.providers[%d].api_key is required", i)
		}
		if p.Dimensions != 0 && p.Dimensions != EmbeddingDimensions {
			return fmt.Errorf("models.providers[%d].dimensions %d does not match the memories.embedding column (vector(%d))",
				i, p.Dimensions, EmbeddingDimensions)
		}
	}

	if _, err := m.ChatProvider(); err != nil {
		return err
	}

	if m.DefaultEmbedding != "" {
		p, err := m.providerByID(m.DefaultEmbedding)
		if err != nil {
			return fmt.Errorf("models.default_embedding: %w", err)
		}
		if !p.IsEmbedding() {
			return fmt.Errorf("models.default_embedding %q refers to a provider without dimensions", m.DefaultEmbedding)
		}
	}
	return nil
}

// ChatProvider resolves the chat provider using the project_plan.md §3 model
// selection rule: the provider named by default_chat, or otherwise the first
// provider that is not an embedding provider.
func (m Models) ChatProvider() (*Provider, error) {
	if len(m.Providers) == 0 {
		return nil, ErrMissingProviders
	}

	if m.DefaultChat != "" {
		p, err := m.providerByID(m.DefaultChat)
		if err != nil {
			return nil, err
		}
		if p.IsEmbedding() {
			return nil, fmt.Errorf("models.default_chat %q refers to an embedding provider", m.DefaultChat)
		}
		return p, nil
	}

	for i := range m.Providers {
		if !m.Providers[i].IsEmbedding() {
			return &m.Providers[i], nil
		}
	}
	return nil, ErrNoChatProvider
}

func (m Models) providerByID(id string) (*Provider, error) {
	for i := range m.Providers {
		if m.Providers[i].ID == id {
			return &m.Providers[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, id)
}

// ResolvePrivateKey returns the GitHub App private key. An explicit path wins,
// then the inline value, then the GITHUB_PRIVATE_KEY environment variable.
func (g GitHub) ResolvePrivateKey() ([]byte, error) {
	if g.PrivateKeyPath != "" {
		raw, err := os.ReadFile(g.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading github.private_key_path %s: %w", g.PrivateKeyPath, err)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil, fmt.Errorf("github.private_key_path %s is empty", g.PrivateKeyPath)
		}
		return raw, nil
	}
	if g.PrivateKeyPEM != "" {
		return []byte(g.PrivateKeyPEM), nil
	}
	if inline := os.Getenv("GITHUB_PRIVATE_KEY"); inline != "" {
		return []byte(inline), nil
	}
	return nil, errors.New("no GitHub App private key configured")
}
