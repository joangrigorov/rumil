package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/embedder"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/snapshot"
	"github.com/joangrigorov/rumil/internal/store"
)

// maxErrorBody bounds how much of a provider's or GitHub's error page is quoted
// back. A key does not live there, but a proxy's error page can be long and the
// terminal is not a log sink.
const maxErrorBody = 300

// configCheck reports what loading and validating the file established. The
// load and validation happen before doctor runs, so reaching this check means
// both succeeded; the detail says what remains for the other checks.
func configCheck(cfg *config.Config, red *redactor) Check {
	if cfg == nil {
		return fail("config", "no configuration was loaded")
	}
	chat, err := cfg.Models.ChatProvider()
	if err != nil {
		return fail("config", red.text(err.Error()))
	}
	detail := fmt.Sprintf("%d model provider(s), default chat %s (%s); %d MCP server(s)",
		len(cfg.Models.Providers), chat.ID, chat.ModelName, len(cfg.MCPServers))
	if memoryOn(cfg) {
		detail += "; memory on"
	} else {
		detail += "; memory off"
	}
	return pass("config", detail)
}

// databaseCheck pings Postgres, compares the applied schema with the migrations
// embedded in this binary, and, when memory is on, confirms pgvector. The
// connection is opened without auto-migration, so the check is read-only.
func databaseCheck(ctx context.Context, cfg *config.Config, red *redactor) Check {
	const name = "database"
	if cfg.Database.DSN == "" {
		if provider, err := cfg.Models.EmbeddingProvider(); err == nil && provider != nil {
			return fail(name, "database.dsn is empty, so the configured embedding provider has nowhere to store rules")
		}
		return skip(name, "database.dsn is empty: no queue and no memory")
	}

	ctx, cancel := checkContext(ctx)
	defer cancel()

	db, err := store.Open(ctx, cfg.Database.DSN, false, 0)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	defer db.Close()

	latest, err := store.LatestMigrationVersion()
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	version, err := db.MigrationVersion(ctx)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	switch {
	case version < latest:
		return fail(name, fmt.Sprintf("connected, but the schema is at version %d of %d; start `rumil serve` with database.auto_migrate on, or apply the migrations", version, latest))
	case version > latest:
		return fail(name, fmt.Sprintf("connected, but the schema is at version %d and this binary embeds %d; upgrade rumil before it writes to this database", version, latest))
	}

	detail := fmt.Sprintf("connected, schema version %d of %d", version, latest)
	if memoryOn(cfg) {
		installed, err := db.HasExtension(ctx, "vector")
		if err != nil {
			return fail(name, red.text(err.Error()))
		}
		if !installed {
			return fail(name, detail+", but the vector extension is not installed")
		}
		detail += ", pgvector installed"
	}
	return pass(name, detail)
}

// githubCheck mints an installation token, reads the installation's own record,
// and with --repo confirms the repository is visible to it.
func githubCheck(ctx context.Context, cfg *config.Config, baseURL, repo string, red *redactor) Check {
	const name = "github"
	ctx, cancel := checkContext(ctx)
	defer cancel()

	keyPEM, err := cfg.GitHub.ResolvePrivateKey()
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	auth, err := ghapp.NewAppAuth(cfg.GitHub, keyPEM, baseURL)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	if err := auth.Verify(ctx); err != nil {
		return fail(name, red.text(err.Error()))
	}
	record, err := auth.Record(ctx)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}

	detail := fmt.Sprintf("installation %d belongs to %q", cfg.GitHub.InstallationID, record.Slug)
	if level := record.Permissions.GetContents(); level != "" {
		detail += fmt.Sprintf(" (contents: %s)", level)
	}

	if strings.TrimSpace(repo) != "" {
		owner, repository, err := splitRepo(repo)
		if err != nil {
			return fail(name, err.Error())
		}
		if _, _, err := auth.Client().Repositories.Get(ctx, owner, repository); err != nil {
			return fail(name, red.text(fmt.Sprintf("%s, but %s/%s is not readable: %v", detail, owner, repository, err)))
		}
		detail += fmt.Sprintf(", and %s/%s is visible", owner, repository)
	}
	return pass(name, detail)
}

// chatCheck sends one small completion and reports its latency and whether the
// provider returned usage. A provider that omits usage still reviews, but the
// token and cost budgets cannot count it, so that is a warning.
func chatCheck(ctx context.Context, cfg *config.Config, red *redactor) Check {
	const name = "chat"
	provider, err := cfg.Models.ChatProvider()
	if err != nil {
		return fail(name, red.text(err.Error()))
	}

	ctx, cancel := checkContext(ctx)
	defer cancel()

	started := time.Now()
	completion, err := complete(ctx, provider)
	latency := time.Since(started).Round(time.Millisecond)
	if err != nil {
		return fail(name, red.text(fmt.Sprintf("%s did not answer: %v", provider.ID, err)))
	}

	detail := fmt.Sprintf("%s (%s) answered in %s", provider.ID, provider.ModelName, latency)
	if completion.Usage == nil {
		return warn(name, detail+", but reported no usage, so token and cost budgets cannot count it")
	}
	detail += fmt.Sprintf(": %d input, %d output tokens", completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
	return pass(name, detail)
}

// embeddingCheck calls the configured embedding provider and confirms the
// vector width its response carried. With no provider, memory is off, which is
// supported but worth saying.
func embeddingCheck(ctx context.Context, cfg *config.Config, red *redactor) Check {
	const name = "embedding"
	provider, err := cfg.Models.EmbeddingProvider()
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	if provider == nil {
		return warn(name, "models.default_embedding is not set, so memory is off and review comments cannot record rules")
	}

	client, err := embedder.New(provider)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	ctx, cancel := checkContext(ctx)
	defer cancel()

	vector, err := client.Embed(ctx, "rumil doctor connectivity check")
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	return pass(name, fmt.Sprintf("%s (%s) returned %d dimensions", provider.ID, provider.ModelName, len(vector)))
}

// snapshotCheck downloads a repository's default branch and extracts it into a
// temporary directory, then removes it. It is opt-in because it is the one
// check that moves a repository's bytes.
func snapshotCheck(ctx context.Context, cfg *config.Config, baseURL, repo string, red *redactor) Check {
	const name = "snapshot"
	if strings.TrimSpace(repo) == "" {
		return skip(name, "pass --snapshot owner/name to download and extract a repository")
	}
	if !cfg.Snapshot.SnapshotEnabled() {
		return skip(name, "snapshot.enabled is false: sessions read through the GitHub API")
	}

	owner, repository, err := splitRepo(repo)
	if err != nil {
		return fail(name, err.Error())
	}

	ctx, cancel := checkContext(ctx)
	defer cancel()

	keyPEM, err := cfg.GitHub.ResolvePrivateKey()
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	auth, err := ghapp.NewAppAuth(cfg.GitHub, keyPEM, baseURL)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	client := auth.Client()

	record, _, err := client.Repositories.Get(ctx, owner, repository)
	if err != nil {
		return fail(name, red.text(fmt.Sprintf("reading %s/%s: %v", owner, repository, err)))
	}
	branch := record.GetDefaultBranch()
	if branch == "" {
		return fail(name, fmt.Sprintf("%s/%s reports no default branch", owner, repository))
	}

	link, _, err := client.Repositories.GetArchiveLink(ctx, owner, repository, github.Tarball,
		&github.RepositoryContentGetOptions{Ref: branch}, 0)
	if err != nil {
		return fail(name, red.text(fmt.Sprintf("asking GitHub for the archive of %s/%s at %s: %v", owner, repository, branch, err)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return fail(name, red.text(err.Error()))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The signed URL authorizes the download; it must not reach the report.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fail(name, red.text(fmt.Sprintf("downloading the archive of %s/%s: %v", owner, repository, err)))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail(name, fmt.Sprintf("downloading the archive of %s/%s: %s", owner, repository, resp.Status))
	}

	extracted, err := snapshot.Create(ctx, resp.Body, snapshot.Options{
		MaxBytes: cfg.Snapshot.MaxBytesOrDefault(),
		Dir:      cfg.Snapshot.Dir,
	})
	if err != nil {
		return fail(name, red.text(fmt.Sprintf("extracting %s/%s@%s: %v", owner, repository, branch, err)))
	}
	files := extracted.Count()
	if err := extracted.Close(); err != nil {
		return fail(name, red.text(fmt.Sprintf("extracted %d files from %s/%s@%s but could not remove the directory: %v",
			files, owner, repository, branch, err)))
	}
	return pass(name, fmt.Sprintf("extracted %d files from %s/%s@%s into a temporary directory and removed it",
		files, owner, repository, branch))
}

// serverCheck reports the credentials `rumil serve` needs. A missing webhook
// secret stops the server from starting; no admin credential only leaves the
// operator surfaces closed.
func serverCheck(cfg *config.Config, red *redactor) Check {
	const name = "server"
	if cfg.Database.DSN == "" {
		return skip(name, "database.dsn is empty, so `rumil serve` has no queue")
	}
	if cfg.GitHub.WebhookSecret == "" {
		return fail(name, "github.webhook_secret is required to run `rumil serve`")
	}
	if len(cfg.Moderation.AdminCredentials()) == 0 {
		return warn(name, "the webhook secret is set, but no admin credential is: /ui/moderation, the deck and /metrics refuse every request. Set moderation.admin_token or add a moderation.admin_tokens entry")
	}
	return pass(name, "webhook secret and admin credential are set")
}

// chatUsage is what a provider says the completion cost.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// chatCompletion is the part of a completion doctor reads.
type chatCompletion struct {
	Usage *chatUsage
}

// complete asks a provider for one short answer. It speaks the
// chat-completions request directly: doctor checks connectivity, and a review
// session would hide the provider's own error behind the agent harness.
func complete(ctx context.Context, provider *config.Provider) (*chatCompletion, error) {
	payload, err := json.Marshal(map[string]any{
		"model": provider.ModelName,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with the single word OK."},
		},
		"max_tokens": 8,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the request: %w", err)
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}

	client := &http.Client{Timeout: provider.RequestTimeoutOrDefault()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("answered %s: %s", resp.Status, errorText(resp.Body))
	}

	var payloadResponse struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *chatUsage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payloadResponse); err != nil {
		return nil, fmt.Errorf("decoding the response: %w", err)
	}
	if len(payloadResponse.Choices) == 0 {
		return nil, errors.New("answered with no completion")
	}
	return &chatCompletion{Usage: payloadResponse.Usage}, nil
}

// errorText reports a failed response in one line, preferring the provider's
// own message and never reading more than maxErrorBody runes into the report.
func errorText(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return "unreadable response body"
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Error.Message != "" {
		return clip(payload.Error.Message)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "empty response body"
	}
	return clip(text)
}

func clip(text string) string {
	if len(text) <= maxErrorBody {
		return text
	}
	return text[:maxErrorBody] + "…"
}

// splitRepo validates an owner/name argument.
func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("--repo and --snapshot take owner/name, got %q", repo)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}
