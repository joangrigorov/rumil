// Package config loads, defaults, and validates the Rumil configuration file.
//
// The schema is the one specified in project_plan.md §3. Loading expands
// ${ENV_VAR} placeholders before the YAML is parsed, so secrets never need to
// live in the file itself.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultEmbeddingDimensions is the width `rumil init` offers and the width an
// embedding provider is taken to have when it states none. Memory runs at the
// width its embedding provider declares, because internal/store sizes
// memories.embedding to that width at startup (project_plan.md §3 rule 5).
const DefaultEmbeddingDimensions = 1536

// MaxEmbeddingDimensions is the widest vector memory can hold: pgvector builds
// the HNSW index memory search uses up to 2000 dimensions.
const MaxEmbeddingDimensions = 2000

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
	// DefaultWorkerConcurrency is the number of reviews `rumil serve` runs in
	// parallel.
	DefaultWorkerConcurrency = 4
	// DefaultWorkerTimeout bounds one review or command job. A hung model call
	// would otherwise hold a worker for the life of the process.
	DefaultWorkerTimeout = 15 * time.Minute
	// DefaultListen is the HTTP address of `rumil serve`.
	DefaultListen = ":8080"

	// DefaultCompactionTriggerTokens is when a session summarizes its older
	// messages. 256K leaves a large working set in place and is well below the
	// million-token context of the models Rumil is usually pointed at.
	DefaultCompactionTriggerTokens = 256 * 1024
	// DefaultCompactionReserveTokens is the output budget kept out of the
	// trigger, so the session can still answer after the context grows.
	DefaultCompactionReserveTokens = 16_384
	// DefaultCompactionKeepRecentTokens is the most recent context that is
	// never summarized.
	DefaultCompactionKeepRecentTokens = 20_000

	// DefaultSnapshotMaxBytes caps the working tree a session materializes. A
	// repository above it falls back to the GitHub API tools.
	DefaultSnapshotMaxBytes int64 = 512 << 20
	// DefaultSnapshotTimeout bounds the archive download and extraction, so a
	// slow origin cannot eat the job's own timeout.
	DefaultSnapshotTimeout = 2 * time.Minute

	// DefaultEmbeddingTimeout bounds one embeddings call. A local backend can
	// spend longer than this on its first request while it loads or compiles
	// the model, which is what models.providers[].embedding_timeout raises.
	DefaultEmbeddingTimeout = 30 * time.Second
)

// GuidelinesRef values accepted by review.guidelines_ref.
const (
	GuidelinesRefBase = "base"
	GuidelinesRefHead = "head"
)

// ThinkingLevel values accepted by models.providers[].thinking_level. They
// mirror kit's levels; "off" leaves the provider's default in place, while
// "none" asks explicitly for no reasoning.
const (
	ThinkingOff     = "off"
	ThinkingNone    = "none"
	ThinkingMinimal = "minimal"
	ThinkingLow     = "low"
	ThinkingMedium  = "medium"
	ThinkingHigh    = "high"
	// ThinkingMax is the highest effort some providers expose, including
	// DeepSeek V4 Flash. It maps to reasoning_effort "max".
	ThinkingMax = "max"
)

// ThinkingLevels is the ordered list of accepted reasoning levels.
var ThinkingLevels = []string{
	ThinkingOff, ThinkingNone, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingMax,
}

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
	Compaction Compaction `yaml:"compaction"`
	Snapshot   Snapshot   `yaml:"snapshot"`
	Memory     Memory     `yaml:"memory"`
	Worker     Worker     `yaml:"worker"`
	Server     Server     `yaml:"server"`
	Models     Models     `yaml:"models"`
	Database   Database   `yaml:"database"`
	Moderation Moderation `yaml:"moderation"`
}

// Compaction controls how a session condenses its older messages when the
// context grows. Rumil triggers it itself at TriggerTokens rather than leaving
// it to the model registry, because the configured OpenAI-compatible model is
// usually absent from kit's catalog and an unknown model never auto-compacts.
type Compaction struct {
	// Enabled is a pointer so that omitting the block turns compaction on,
	// while an explicit false turns it off.
	Enabled *bool `yaml:"enabled"`
	// TriggerTokens is the estimated context size at which older messages are
	// summarized. Nothing is compacted before this point.
	TriggerTokens int `yaml:"trigger_tokens"`
	// ReserveTokens is the output budget kept below TriggerTokens.
	ReserveTokens int `yaml:"reserve_tokens"`
	// KeepRecentTokens is the most recent context kept verbatim.
	KeepRecentTokens int `yaml:"keep_recent_tokens"`
}

// CompactionEnabled reports whether sessions compact their context. It is on
// unless the operator set compaction.enabled to false.
func (c Compaction) CompactionEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Snapshot controls the per-session working tree the reviewer reads from. The
// tree is downloaded from the reviewed commit, extracted to a temporary
// directory, and removed when the session ends. It is what makes listing,
// searching and reading files local and fast instead of an API call per guess.
type Snapshot struct {
	// Enabled is a pointer so that omitting the block turns the snapshot on,
	// while an explicit false turns it off and leaves the GitHub API tools
	// alone.
	Enabled *bool `yaml:"enabled"`
	// MaxBytes caps the extracted working tree. A larger repository falls back
	// to the GitHub API tools rather than filling the disk.
	MaxBytes int64 `yaml:"max_bytes"`
	// Timeout bounds the download and extraction, as a Go duration such as
	// "2m". Empty uses DefaultSnapshotTimeout.
	Timeout string `yaml:"timeout"`
	// Dir is the parent directory for the per-session working trees. Empty
	// uses the operating system's temporary directory.
	Dir string `yaml:"dir"`
}

// SnapshotEnabled reports whether sessions materialize a working tree. It is on
// unless the operator set snapshot.enabled to false.
func (s Snapshot) SnapshotEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// MaxBytesOrDefault is the working-tree cap, defaulted when the operator left
// it out.
func (s Snapshot) MaxBytesOrDefault() int64 {
	if s.MaxBytes <= 0 {
		return DefaultSnapshotMaxBytes
	}
	return s.MaxBytes
}

// TimeoutOrDefault is the download and extraction budget, defaulted when the
// operator left it out.
func (s Snapshot) TimeoutOrDefault() time.Duration {
	if s.Timeout == "" {
		return DefaultSnapshotTimeout
	}
	timeout, err := time.ParseDuration(s.Timeout)
	if err != nil {
		return DefaultSnapshotTimeout
	}
	return timeout
}

// Worker tunes the review job pool of `rumil serve`.
type Worker struct {
	Concurrency int `yaml:"concurrency"`
	// JobTimeout bounds one review or command job, as a Go duration string
	// such as "15m" or "1h". Empty uses DefaultWorkerTimeout.
	JobTimeout string `yaml:"job_timeout"`
}

// Timeout is the wall-clock budget for one job.
func (w Worker) Timeout() time.Duration {
	if w.JobTimeout == "" {
		return DefaultWorkerTimeout
	}
	timeout, err := time.ParseDuration(w.JobTimeout)
	if err != nil {
		return DefaultWorkerTimeout
	}
	return timeout
}

// Server holds the HTTP surface settings of `rumil serve`.
type Server struct {
	Listen string `yaml:"listen"`
}

// App holds process-wide behavior switches.
type App struct {
	BotHandle string `yaml:"bot_handle"`
	// MaxAgentTurns is a pointer so that omitted keeps the default while an
	// explicit 0 means no limit.
	MaxAgentTurns *int   `yaml:"max_agent_turns"`
	MaxDiffBytes  int    `yaml:"max_diff_bytes"`
	LogLevel      string `yaml:"log_level"`
}

// MaxTurns is the number of agent steps one session may take. Zero means the
// session runs until the model stops or the job timeout arrives.
func (a App) MaxTurns() int {
	if a.MaxAgentTurns == nil {
		return DefaultMaxAgentTurns
	}
	return *a.MaxAgentTurns
}

// TurnsUnlimited reports whether the operator removed the agent-turn cap.
func (a App) TurnsUnlimited() bool { return a.MaxTurns() == 0 }

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
	// EmbeddingTimeout bounds one embeddings call, as a Go duration string
	// such as "90s". It applies to an embedding provider; empty uses
	// DefaultEmbeddingTimeout.
	EmbeddingTimeout string `yaml:"embedding_timeout"`
	// ThinkingLevel is the reasoning effort sent to a chat model. Empty leaves
	// the provider default in place.
	ThinkingLevel string `yaml:"thinking_level"`
}

// IsEmbedding reports whether the provider produces embeddings.
func (p Provider) IsEmbedding() bool { return p.Dimensions > 0 }

// EmbeddingTimeoutOrDefault is the budget of one embeddings call, defaulted
// when the operator left it out.
func (p Provider) EmbeddingTimeoutOrDefault() time.Duration {
	if p.EmbeddingTimeout == "" {
		return DefaultEmbeddingTimeout
	}
	timeout, err := time.ParseDuration(p.EmbeddingTimeout)
	if err != nil {
		return DefaultEmbeddingTimeout
	}
	return timeout
}

// Review tunes the repository guidelines injected into the review prompt and
// the reviewer's own write actions.
type Review struct {
	GuidelinesRef string `yaml:"guidelines_ref"`
	// ResolveThreads lets the reviewer resolve the review threads it opened. It
	// is a pointer so that an operator can turn it off explicitly, since the
	// default is on and a plain bool could not tell the two apart.
	ResolveThreads *bool `yaml:"resolve_threads"`
}

// ThreadsResolvable reports whether the reviewer may resolve its own review
// threads. It may, unless the operator turned that off.
func (r Review) ThreadsResolvable() bool {
	return r.ResolveThreads == nil || *r.ResolveThreads
}

// Memory tunes how recorded rules are ranked (used from v0.0.6).
type Memory struct {
	// RepoScopeBoost is added to a REPO-scoped rule's similarity when the
	// candidates for a review are ranked, so a project-specific rule outranks a
	// general organization rule that came back at a similar distance.
	RepoScopeBoost *float64 `yaml:"repo_scope_boost"`
}

// DefaultRepoScopeBoost is the weight a repository's own rule gets over an
// organization rule. It is small on purpose: a repository exception should win
// a tie against the general rule, not outrank a rule that is a much better
// match.
const DefaultRepoScopeBoost = 0.05

// RepoScopeWeight is the boost the queries rank with, defaulted when the
// operator left it out.
func (m Memory) RepoScopeWeight() float64 {
	if m.RepoScopeBoost == nil {
		return DefaultRepoScopeBoost
	}
	return *m.RepoScopeBoost
}

// Database is used from v0.0.3 on.
type Database struct {
	DSN         string `yaml:"dsn"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// Moderation is used from v0.0.4 on.
type Moderation struct {
	AcceptAll               bool     `yaml:"accept_all"`
	AutoApproveAssociations []string `yaml:"auto_approve_associations"`
	AdminToken              string   `yaml:"admin_token"`
}

// AutoApproves reports whether a commenter with this GitHub
// author_association may record a rule without a maintainer looking at it.
// accept_all answers for everybody; the list answers per association, which is
// how a team lets its own members record rules while strangers queue for
// moderation.
func (m Moderation) AutoApproves(association string) bool {
	if m.AcceptAll {
		return true
	}
	association = strings.TrimSpace(association)
	if association == "" {
		// No association is on the list: a delivery that carries none, and an
		// empty entry someone left in the list, must not agree with each other.
		return false
	}
	for _, approved := range m.AutoApproveAssociations {
		approved = strings.TrimSpace(approved)
		if approved != "" && strings.EqualFold(approved, association) {
			return true
		}
	}
	return false
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
		return nil, fmt.Errorf("invalid config in %s: %w", source, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.App.BotHandle == "" {
		c.App.BotHandle = DefaultBotHandle
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
	if c.Worker.Concurrency == 0 {
		c.Worker.Concurrency = DefaultWorkerConcurrency
	}
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Compaction.TriggerTokens == 0 {
		c.Compaction.TriggerTokens = DefaultCompactionTriggerTokens
	}
	if c.Compaction.ReserveTokens == 0 {
		c.Compaction.ReserveTokens = DefaultCompactionReserveTokens
	}
	if c.Compaction.KeepRecentTokens == 0 {
		c.Compaction.KeepRecentTokens = DefaultCompactionKeepRecentTokens
	}
}

// Validate enforces the schema rules that v0.0.1 depends on: app limits, GitHub
// App credential presence, and the model selection rules.
func (c *Config) Validate() error {
	if c.App.MaxTurns() < 0 {
		return fmt.Errorf("app.max_agent_turns must be 0 for unlimited or at least 1, got %d", c.App.MaxTurns())
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

	if c.Worker.Concurrency < 1 {
		return fmt.Errorf("worker.concurrency must be at least 1, got %d", c.Worker.Concurrency)
	}
	if c.Worker.JobTimeout != "" {
		timeout, err := time.ParseDuration(c.Worker.JobTimeout)
		if err != nil {
			return fmt.Errorf("worker.job_timeout must be a duration such as \"15m\" or \"1h\", got %q: %w",
				c.Worker.JobTimeout, err)
		}
		if timeout <= 0 {
			return fmt.Errorf("worker.job_timeout must be positive, got %q", c.Worker.JobTimeout)
		}
	}

	if c.Compaction.CompactionEnabled() {
		if c.Compaction.TriggerTokens < 1 {
			return fmt.Errorf("compaction.trigger_tokens must be at least 1, got %d", c.Compaction.TriggerTokens)
		}
		if c.Compaction.ReserveTokens < 1 {
			return fmt.Errorf("compaction.reserve_tokens must be at least 1, got %d", c.Compaction.ReserveTokens)
		}
		if c.Compaction.KeepRecentTokens < 1 {
			return fmt.Errorf("compaction.keep_recent_tokens must be at least 1, got %d", c.Compaction.KeepRecentTokens)
		}
		// Keeping as much as the trigger leaves nothing to summarize and would
		// compact on every later step without reducing the context.
		if c.Compaction.KeepRecentTokens >= c.Compaction.TriggerTokens {
			return fmt.Errorf("compaction.keep_recent_tokens must be less than compaction.trigger_tokens (%d), got %d",
				c.Compaction.TriggerTokens, c.Compaction.KeepRecentTokens)
		}
		if c.Compaction.ReserveTokens >= c.Compaction.TriggerTokens {
			return fmt.Errorf("compaction.reserve_tokens must be less than compaction.trigger_tokens (%d), got %d",
				c.Compaction.TriggerTokens, c.Compaction.ReserveTokens)
		}
	}

	if c.Snapshot.SnapshotEnabled() {
		if c.Snapshot.MaxBytes < 0 {
			return fmt.Errorf("snapshot.max_bytes must be positive or zero for the default, got %d", c.Snapshot.MaxBytes)
		}
		if c.Snapshot.Timeout != "" {
			timeout, err := time.ParseDuration(c.Snapshot.Timeout)
			if err != nil {
				return fmt.Errorf("snapshot.timeout must be a duration such as \"2m\" or \"90s\", got %q: %w",
					c.Snapshot.Timeout, err)
			}
			if timeout <= 0 {
				return fmt.Errorf("snapshot.timeout must be positive, got %q", c.Snapshot.Timeout)
			}
		}
	}

	// A negative weight would rank an organization rule above the repository's
	// own, which is the opposite of what the setting is for, and a weight above
	// one would let any repository rule outrank every organization rule
	// whatever the distance. A NaN is refused too: it compares false against
	// both bounds, and Postgres orders it above every number, which would rank
	// every repository rule first.
	if boost := c.Memory.RepoScopeWeight(); math.IsNaN(boost) || boost < 0 || boost > 1 {
		return fmt.Errorf("memory.repo_scope_boost must be between 0 and 1, got %v", boost)
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

// validThinkingLevel reports whether level is empty or one of ThinkingLevels.
func validThinkingLevel(level string) bool {
	if level == "" {
		return true
	}
	for _, accepted := range ThinkingLevels {
		if level == accepted {
			return true
		}
	}
	return false
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
		if p.Dimensions < 0 || p.Dimensions > MaxEmbeddingDimensions {
			return fmt.Errorf("models.providers[%d].dimensions %d is outside 1-%d, the range pgvector indexes, and 0 to mark a provider as the chat model",
				i, p.Dimensions, MaxEmbeddingDimensions)
		}
		if !validThinkingLevel(p.ThinkingLevel) {
			return fmt.Errorf("models.providers[%d].thinking_level must be one of %s, got %q",
				i, strings.Join(ThinkingLevels, ", "), p.ThinkingLevel)
		}
		if p.EmbeddingTimeout != "" {
			if !p.IsEmbedding() {
				return fmt.Errorf("models.providers[%d].embedding_timeout applies to an embedding provider, but %q declares no dimensions",
					i, p.ID)
			}
			timeout, err := time.ParseDuration(p.EmbeddingTimeout)
			if err != nil {
				return fmt.Errorf("models.providers[%d].embedding_timeout must be a duration such as \"90s\" or \"2m\", got %q: %w",
					i, p.EmbeddingTimeout, err)
			}
			if timeout <= 0 {
				return fmt.Errorf("models.providers[%d].embedding_timeout must be positive, got %q", i, p.EmbeddingTimeout)
			}
		}
	}

	// One column backs memory, so two embedding providers have to agree on its
	// width. Only the selected one is used, and a disagreement would otherwise
	// surface at startup as a column sized to the other provider.
	var width int
	var widthID string
	for i, p := range m.Providers {
		if !p.IsEmbedding() {
			continue
		}
		if width == 0 {
			width, widthID = p.Dimensions, p.ID
			continue
		}
		if p.Dimensions != width {
			return fmt.Errorf("models.providers[%d].dimensions %d disagrees with %q (%d): one column backs memory, so every embedding provider states the same width",
				i, p.Dimensions, widthID, width)
		}
	}

	if _, err := m.ChatProvider(); err != nil {
		return err
	}

	if _, err := m.EmbeddingProvider(); err != nil {
		return err
	}
	return nil
}

// EmbeddingProvider resolves the provider named by models.default_embedding. It
// returns nil, nil when that selector is unset, because memory is opt-in: an
// absent embedding provider means no pre-retrieval and no memory tools rather
// than a configuration error (project_plan.md §3 rule 6).
func (m Models) EmbeddingProvider() (*Provider, error) {
	if m.DefaultEmbedding == "" {
		return nil, nil
	}
	p, err := m.providerByID(m.DefaultEmbedding)
	if err != nil {
		return nil, fmt.Errorf("models.default_embedding: %w", err)
	}
	if !p.IsEmbedding() {
		return nil, fmt.Errorf("models.default_embedding %q refers to a provider without dimensions", m.DefaultEmbedding)
	}
	return p, nil
}

// EmbeddingDimensions is the width the selected embedding provider declares,
// and zero when memory is off. internal/store sizes memories.embedding to it,
// so the width is decided here and nowhere else.
//
// A configuration that names an unusable embedding provider is refused by
// validate before this is asked, so an error here resolves to the zero width
// rather than being reported twice.
func (m Models) EmbeddingDimensions() int {
	p, err := m.EmbeddingProvider()
	if err != nil || p == nil {
		return 0
	}
	return p.Dimensions
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
