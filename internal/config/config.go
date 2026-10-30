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
	"net"
	"net/url"
	"os"
	"regexp"
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

	// DefaultRequestTimeout bounds how long a chat provider may take to start
	// answering. A provider that queues a request behind its own load can sit
	// silent for minutes; the header timeout fails the attempt so the retry
	// policy can try again instead of holding a worker.
	DefaultRequestTimeout = 2 * time.Minute
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
	App        App         `yaml:"app"`
	GitHub     GitHub      `yaml:"github"`
	Review     Review      `yaml:"review"`
	Compaction Compaction  `yaml:"compaction"`
	Snapshot   Snapshot    `yaml:"snapshot"`
	Memory     Memory      `yaml:"memory"`
	Worker     Worker      `yaml:"worker"`
	Server     Server      `yaml:"server"`
	MCPServers []MCPServer `yaml:"mcp_servers"`
	Models     Models      `yaml:"models"`
	Database   Database    `yaml:"database"`
	Moderation Moderation  `yaml:"moderation"`
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
	// PublicURL is the address a reviewer reaches the deck on. It is what a
	// GitHub check run links to; empty leaves the check without a details link.
	PublicURL string `yaml:"public_url"`
	// MetricsListen is the address of a separate metrics listener. It is
	// empty by default: /metrics then lives on the main listener behind the
	// admin token. A separate listener is meant for a loopback or firewalled
	// address that a scraper reaches without the credential.
	MetricsListen string `yaml:"metrics_listen"`
}

// MCPServer is one operator-provided MCP server whose tools a review may use.
// The allow list is the trust boundary: tools run with the service account's
// privileges, outside any sandbox Rumil provides.
type MCPServer struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // stdio | http
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	Env       map[string]string `yaml:"env"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	Timeout   string            `yaml:"timeout"`
	// MaxOutputBytes caps one tool result. A larger result is cut at a rune
	// boundary with a marker, so one call cannot fill the session's context.
	MaxOutputBytes int `yaml:"max_output_bytes"`
	// Tools says which of the server's tools are exposed to the model. An empty
	// list exposes none: the operator names every tool they trust.
	Tools MCPToolPolicy `yaml:"tools"`
}

// MCPToolPolicy is the allow list for one MCP server.
type MCPToolPolicy struct {
	Allow []string `yaml:"allow"`
}

// DefaultMCPTimeout bounds a call to an operator's MCP server.
const DefaultMCPTimeout = 60 * time.Second

// DefaultMCPMaxOutputBytes caps one MCP tool result.
const DefaultMCPMaxOutputBytes = 64 << 10

// MaxMCPMaxOutputBytes is the largest result an operator may allow.
const MaxMCPMaxOutputBytes = 1 << 20

// TimeoutOrDefault is the call timeout, or the default when unset.
func (m MCPServer) TimeoutOrDefault() time.Duration {
	if m.Timeout == "" {
		return DefaultMCPTimeout
	}
	timeout, err := time.ParseDuration(m.Timeout)
	if err != nil {
		return DefaultMCPTimeout
	}
	return timeout
}

// MaxOutputOrDefault is the result cap, or the default when unset.
func (m MCPServer) MaxOutputOrDefault() int {
	if m.MaxOutputBytes <= 0 {
		return DefaultMCPMaxOutputBytes
	}
	return m.MaxOutputBytes
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
	// Fallback is the ordered list of chat providers a review tries after the
	// selected provider fails on transport errors, 5xx, repeated 429 or a
	// response-header timeout. It is chat only: an embedding failure degrades
	// memory without failing the review.
	Fallback []string `yaml:"fallback"`
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
	// RequestTimeout bounds how long a chat provider may take to send the
	// response headers, as a Go duration string. It applies to a chat
	// provider; empty uses DefaultRequestTimeout.
	RequestTimeout string `yaml:"request_timeout"`
	// ThinkingLevel is the reasoning effort sent to a chat model. Empty leaves
	// the provider default in place.
	ThinkingLevel string `yaml:"thinking_level"`
	// The three prices are what a million tokens cost on this provider. They
	// are optional: unset means the cost of that kind of token is unknown, and
	// no estimated cost is reported until at least the input and output prices
	// are set.
	PriceInputPerMTok     *float64 `yaml:"price_input_per_mtok"`
	PriceOutputPerMTok    *float64 `yaml:"price_output_per_mtok"`
	PriceCacheReadPerMTok *float64 `yaml:"price_cache_read_per_mtok"`
}

// TokenPrice is the cost of one million tokens of each kind. A kind the
// operator did not price counts as zero.
type TokenPrice struct {
	Input     float64
	Output    float64
	CacheRead float64
}

// TokenPrice returns the configured rates, leaving an unpriced kind at zero.
func (p Provider) TokenPrice() TokenPrice {
	price := TokenPrice{}
	if p.PriceInputPerMTok != nil {
		price.Input = *p.PriceInputPerMTok
	}
	if p.PriceOutputPerMTok != nil {
		price.Output = *p.PriceOutputPerMTok
	}
	if p.PriceCacheReadPerMTok != nil {
		price.CacheRead = *p.PriceCacheReadPerMTok
	}
	return price
}

// HasPrice reports whether the two rates that dominate a bill are set, which is
// what makes an estimated cost meaningful.
func (p Provider) HasPrice() bool {
	return p.PriceInputPerMTok != nil && p.PriceOutputPerMTok != nil
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

// RequestTimeoutOrDefault is how long the provider may take to start
// answering, defaulted when the operator left it out.
func (p Provider) RequestTimeoutOrDefault() time.Duration {
	if p.RequestTimeout == "" {
		return DefaultRequestTimeout
	}
	timeout, err := time.ParseDuration(p.RequestTimeout)
	if err != nil {
		return DefaultRequestTimeout
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
	// MaxTotalTokens bounds the tokens one session may use. The session stops
	// before its next step once the reported usage passes it. Zero means no
	// token budget.
	MaxTotalTokens int64 `yaml:"max_total_tokens"`
	// MaxCostUSD bounds the estimated cost one session may use, from the chat
	// provider's prices. Zero means no cost budget.
	MaxCostUSD float64 `yaml:"max_cost_usd"`
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
	// AdminToken is the single credential operators used before named
	// credentials existed. It is accepted as an entry named "legacy".
	AdminToken string `yaml:"admin_token"`
	// AdminTokens are the named credentials the operator surfaces accept.
	// Every one is compared on every request, and the name of the matching
	// entry is what the audit trail records.
	AdminTokens []AdminCredential `yaml:"admin_tokens"`
}

// AdminCredential is one named credential for the moderation page, the deck and
// the metrics endpoint. The name identifies the holder in the audit trail; the
// token is the secret itself.
type AdminCredential struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
}

// maxAdminCredentials bounds the constant-time comparisons one admin request
// makes. Sixteen named holders is a team, not a directory.
const maxAdminCredentials = 16

// adminCredentialName is what a name may contain. It is rendered in the audit
// trail and on the pages, so it is a plain handle rather than free text.
var adminCredentialName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// AdminCredentials is the effective credential set. A lone admin_token is an
// entry named "legacy", which keeps a configuration written before named
// credentials worked without changing it.
func (m Moderation) AdminCredentials() []AdminCredential {
	credentials := make([]AdminCredential, 0, len(m.AdminTokens)+1)
	if m.AdminToken != "" {
		credentials = append(credentials, AdminCredential{Name: "legacy", Token: m.AdminToken})
	}
	return append(credentials, m.AdminTokens...)
}

// validateCredentials refuses a set that cannot identify one holder: a missing
// name, a name that is not a handle, a duplicated name or a duplicated token.
// The duplicate token check is what keeps the audit name unambiguous.
func (m Moderation) validateCredentials() error {
	if len(m.AdminCredentials()) > maxAdminCredentials {
		return fmt.Errorf("moderation.admin_token and moderation.admin_tokens name %d credentials, want at most %d",
			len(m.AdminCredentials()), maxAdminCredentials)
	}

	names := map[string]bool{}
	tokens := map[string]bool{}
	if m.AdminToken != "" {
		names["legacy"] = true
		tokens[m.AdminToken] = true
	}
	for i, credential := range m.AdminTokens {
		name := strings.TrimSpace(credential.Name)
		if name == "" {
			return fmt.Errorf("moderation.admin_tokens[%d].name is required", i)
		}
		if !adminCredentialName.MatchString(name) {
			return fmt.Errorf("moderation.admin_tokens[%d].name must be 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit, got %q",
				i, credential.Name)
		}
		if names[name] {
			return fmt.Errorf("moderation.admin_tokens has two credentials named %q", name)
		}
		if strings.TrimSpace(credential.Token) == "" {
			return fmt.Errorf("moderation.admin_tokens[%d].token is required", i)
		}
		if tokens[credential.Token] {
			return fmt.Errorf("moderation.admin_tokens[%d].token repeats another credential's token, so the audit trail could not name the holder", i)
		}
		names[name] = true
		tokens[credential.Token] = true
	}
	return nil
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
	c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")
	for i := range c.MCPServers {
		if c.MCPServers[i].Transport == "" {
			c.MCPServers[i].Transport = "stdio"
		}
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

// maxMCPServers bounds the operator-provided tools a session may carry. Every
// advertised tool schema costs prompt budget on every request.
const maxMCPServers = 20

// mcpServerNamePattern keeps a server name usable in an advertised tool name.
var mcpServerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// validateMCPServers checks the operator's servers. A stdio server needs a
// command, an http server needs an address, and both need a name that can be
// prefixed onto a tool the model can call.
func validateMCPServers(servers []MCPServer) error {
	if len(servers) > maxMCPServers {
		return fmt.Errorf("mcp_servers holds %d servers, want at most %d", len(servers), maxMCPServers)
	}
	seen := make(map[string]bool, len(servers))
	for i, server := range servers {
		name := strings.TrimSpace(server.Name)
		if !mcpServerNamePattern.MatchString(name) {
			return fmt.Errorf("mcp_servers[%d].name must be a lowercase handle of letters, digits, - or _, at most 32 characters, got %q", i, server.Name)
		}
		if seen[name] {
			return fmt.Errorf("mcp_servers has two servers named %q", name)
		}
		seen[name] = true

		switch server.Transport {
		case "stdio":
			if strings.TrimSpace(server.Command) == "" {
				return fmt.Errorf("mcp_servers[%d] (%s) is stdio and needs a command", i, name)
			}
			if server.URL != "" {
				return fmt.Errorf("mcp_servers[%d] (%s) is stdio and cannot have a url", i, name)
			}
		case "http":
			if strings.TrimSpace(server.URL) == "" {
				return fmt.Errorf("mcp_servers[%d] (%s) is http and needs a url", i, name)
			}
			parsed, err := url.Parse(server.URL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("mcp_servers[%d] (%s).url must be an absolute http or https URL, got %q", i, name, server.URL)
			}
			if server.Command != "" {
				return fmt.Errorf("mcp_servers[%d] (%s) is http and cannot have a command", i, name)
			}
		default:
			return fmt.Errorf("mcp_servers[%d] (%s).transport must be stdio or http, got %q", i, name, server.Transport)
		}

		if server.Timeout != "" {
			timeout, err := time.ParseDuration(server.Timeout)
			if err != nil || timeout <= 0 {
				return fmt.Errorf("mcp_servers[%d] (%s).timeout must be a positive duration such as \"60s\", got %q", i, name, server.Timeout)
			}
		}
		if server.MaxOutputBytes < 0 || server.MaxOutputBytes > MaxMCPMaxOutputBytes {
			return fmt.Errorf("mcp_servers[%d] (%s).max_output_bytes must be between 1 and %d, got %d",
				i, name, MaxMCPMaxOutputBytes, server.MaxOutputBytes)
		}
		for _, tool := range server.Tools.Allow {
			if strings.TrimSpace(tool) == "" {
				return fmt.Errorf("mcp_servers[%d] (%s).tools.allow holds an empty tool name", i, name)
			}
		}
	}
	return nil
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

	if c.Server.PublicURL != "" {
		parsed, err := url.Parse(c.Server.PublicURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("server.public_url must be an absolute http or https URL, got %q", c.Server.PublicURL)
		}
	}
	if c.Server.MetricsListen != "" {
		// A listener with no port would bind a random one, which a scraper
		// cannot discover from the configuration.
		if _, _, err := net.SplitHostPort(c.Server.MetricsListen); err != nil {
			return fmt.Errorf("server.metrics_listen must be an address such as 127.0.0.1:9090, got %q: %w",
				c.Server.MetricsListen, err)
		}
	}
	if err := validateMCPServers(c.MCPServers); err != nil {
		return err
	}

	switch c.Review.GuidelinesRef {
	case GuidelinesRefBase, GuidelinesRefHead:
	default:
		return fmt.Errorf("review.guidelines_ref must be %q or %q, got %q", GuidelinesRefBase, GuidelinesRefHead, c.Review.GuidelinesRef)
	}

	if c.Review.MaxTotalTokens < 0 {
		return fmt.Errorf("review.max_total_tokens must be zero for no budget or positive, got %d", c.Review.MaxTotalTokens)
	}
	if math.IsNaN(c.Review.MaxCostUSD) || c.Review.MaxCostUSD < 0 {
		return fmt.Errorf("review.max_cost_usd must be zero for no budget or positive, got %v", c.Review.MaxCostUSD)
	}
	if c.Review.MaxCostUSD > 0 {
		// A cost budget the prices cannot evaluate would either never fire or
		// fire at a number nobody can reproduce, so require the two rates that
		// dominate a bill.
		provider, err := c.Models.ChatProvider()
		if err != nil {
			return err
		}
		if !provider.HasPrice() {
			return fmt.Errorf("review.max_cost_usd needs models.providers[%s].price_input_per_mtok and price_output_per_mtok to estimate a cost",
				provider.ID)
		}
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

	if err := c.Moderation.validateCredentials(); err != nil {
		return err
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
		for name, value := range map[string]*float64{
			"price_input_per_mtok":      p.PriceInputPerMTok,
			"price_output_per_mtok":     p.PriceOutputPerMTok,
			"price_cache_read_per_mtok": p.PriceCacheReadPerMTok,
		} {
			if value != nil && (math.IsNaN(*value) || *value < 0) {
				return fmt.Errorf("models.providers[%d].%s must be zero or positive, got %v", i, name, *value)
			}
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
		if p.RequestTimeout != "" {
			if p.IsEmbedding() {
				return fmt.Errorf("models.providers[%d].request_timeout applies to a chat provider, but %q declares dimensions; use embedding_timeout for its embeddings calls",
					i, p.ID)
			}
			timeout, err := time.ParseDuration(p.RequestTimeout)
			if err != nil {
				return fmt.Errorf("models.providers[%d].request_timeout must be a duration such as \"90s\" or \"2m\", got %q: %w",
					i, p.RequestTimeout, err)
			}
			if timeout <= 0 {
				return fmt.Errorf("models.providers[%d].request_timeout must be positive, got %q", i, p.RequestTimeout)
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

	if len(m.Fallback) > maxFallbackProviders {
		return fmt.Errorf("models.fallback holds %d providers, at most %d are allowed", len(m.Fallback), maxFallbackProviders)
	}
	seenFallback := make(map[string]struct{}, len(m.Fallback))
	for i, id := range m.Fallback {
		if _, err := m.ChatProviderByID(id); err != nil {
			return fmt.Errorf("models.fallback[%d]: %w", i, err)
		}
		if _, dup := seenFallback[id]; dup {
			return fmt.Errorf("models.fallback[%d] %q is duplicated", i, id)
		}
		seenFallback[id] = struct{}{}
	}

	if _, err := m.EmbeddingProvider(); err != nil {
		return err
	}
	return nil
}

// maxFallbackProviders bounds the ordered fallback list. Every entry is one
// more attempt a single review may spend, so the list is a budget.
const maxFallbackProviders = 5

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

// ChatProviderByID resolves one configured chat provider by id. A repository
// may name a provider in its own settings, so the lookup has to refuse an id
// the operator did not configure and an embedding provider alike.
func (m Models) ChatProviderByID(id string) (*Provider, error) {
	if id == "" {
		return m.ChatProvider()
	}
	p, err := m.providerByID(id)
	if err != nil {
		return nil, err
	}
	if p.IsEmbedding() {
		return nil, fmt.Errorf("provider %q is an embedding provider, not a chat provider", id)
	}
	return p, nil
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
