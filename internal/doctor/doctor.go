// Package doctor checks one Rumil installation against the services it depends
// on: the configuration, Postgres and its schema, the GitHub App, the chat and
// embedding providers, and the server's credentials.
//
// Every check is independent where it can be: a missing embedding provider is a
// warning rather than a failure, because memory is optional. A check that a
// configured component needs — a chat provider that cannot answer, a schema
// that is behind the binary — is a failure, and Report.Failed is what maps it to
// a non-zero exit.
//
// Nothing here writes to Postgres, creates a review job, or submits a review.
// The database is opened without migrations and queried with SELECTs.
package doctor

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
)

// Level is how a check ended.
type Level string

const (
	// Pass means the component answered as configured.
	Pass Level = "pass"
	// Warn means something optional is missing or worth knowing about. A
	// warning does not fail the run.
	Warn Level = "warn"
	// Fail means a configured component did not work. It fails the run.
	Fail Level = "fail"
	// Skip means the check did not apply, such as a snapshot check without a
	// repository to download.
	Skip Level = "skip"
)

// checkTimeout bounds one network or database check, so a provider that accepts
// a connection and never answers cannot hold the command open indefinitely.
const checkTimeout = 60 * time.Second

// Check is the outcome of one component.
type Check struct {
	Name   string `json:"name"`
	Status Level  `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Setting is one effective, non-secret configuration value. The list is ordered
// the way it is printed, because a map would not carry an order into JSON.
type Setting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Report is everything doctor found.
type Report struct {
	ConfigPath string    `json:"config_path,omitempty"`
	Settings   []Setting `json:"settings"`
	Checks     []Check   `json:"checks"`
}

// Failed reports whether a check that matters failed. Warnings and skips do
// not: a warning is something the operator may decide to live with, and a skip
// is a component that is not configured.
func (r Report) Failed() bool {
	for _, check := range r.Checks {
		if check.Status == Fail {
			return true
		}
	}
	return false
}

// Counts returns how many checks ended at each level.
func (r Report) Counts() map[Level]int {
	counts := map[Level]int{}
	for _, check := range r.Checks {
		counts[check.Status]++
	}
	return counts
}

// Options selects what doctor checks. Config must have been loaded and
// validated; the checks that need the credentials read them from it.
type Options struct {
	Config *config.Config
	// ConfigPath is the file that was read, reported so an operator can tell
	// which of several files won discovery.
	ConfigPath string
	// GitHubAPIBaseURL overrides the GitHub API endpoint and is empty outside
	// tests.
	GitHubAPIBaseURL string
	// Repo is owner/name to confirm the installation can see. Empty skips the
	// visibility read while still checking the App credentials.
	Repo string
	// SnapshotRepo is owner/name whose default branch is downloaded and
	// extracted. Empty skips the check.
	SnapshotRepo string
}

// Run performs every check and returns the report. It does not stop at the
// first failure: an operator fixing three things at once should learn all three
// from one run.
func Run(ctx context.Context, opts Options) Report {
	red := newRedactor(opts.Config)

	report := Report{
		ConfigPath: opts.ConfigPath,
		Settings:   settingsOf(opts.Config),
	}
	// An endpoint can carry credentials in its userinfo even though a provider
	// URL normally does not, so the settings pass through the same redactor.
	for i := range report.Settings {
		report.Settings[i].Value = red.text(report.Settings[i].Value)
	}
	report.Checks = []Check{
		configCheck(opts.Config, red),
		databaseCheck(ctx, opts.Config, red),
		githubCheck(ctx, opts.Config, opts.GitHubAPIBaseURL, opts.Repo, red),
		chatCheck(ctx, opts.Config, red),
		embeddingCheck(ctx, opts.Config, red),
		snapshotCheck(ctx, opts.Config, opts.GitHubAPIBaseURL, opts.SnapshotRepo, red),
		serverCheck(opts.Config, red),
	}
	return report
}

func pass(name, detail string) Check { return Check{Name: name, Status: Pass, Detail: detail} }
func warn(name, detail string) Check { return Check{Name: name, Status: Warn, Detail: detail} }
func fail(name, detail string) Check { return Check{Name: name, Status: Fail, Detail: detail} }
func skip(name, detail string) Check { return Check{Name: name, Status: Skip, Detail: detail} }

func checkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, checkTimeout)
}

// settingsOf is the effective configuration an operator cannot see by reading
// the file, because defaults and provider resolution filled it in. No secret is
// part of it: keys and tokens are reported by presence only.
func settingsOf(cfg *config.Config) []Setting {
	settings := []Setting{
		{Name: "bot handle", Value: cfg.App.BotHandle},
		{Name: "log level", Value: cfg.App.LogLevel},
	}

	if provider, err := cfg.Models.ChatProvider(); err == nil {
		settings = append(settings,
			Setting{Name: "chat provider", Value: provider.ID},
			Setting{Name: "chat model", Value: provider.ModelName},
			Setting{Name: "chat endpoint", Value: provider.BaseURL},
		)
	}
	if len(cfg.Models.Fallback) > 0 {
		settings = append(settings, Setting{Name: "fallback providers", Value: strings.Join(cfg.Models.Fallback, ", ")})
	}

	if provider, err := cfg.Models.EmbeddingProvider(); err == nil && provider != nil {
		settings = append(settings,
			Setting{Name: "embedding provider", Value: provider.ID},
			Setting{Name: "embedding model", Value: provider.ModelName},
			Setting{Name: "embedding dimensions", Value: strconv.Itoa(provider.Dimensions)},
		)
	} else {
		settings = append(settings, Setting{Name: "embedding provider", Value: "off"})
	}

	database := "not configured"
	if cfg.Database.DSN != "" {
		database = "configured"
	}
	settings = append(settings,
		Setting{Name: "database", Value: database},
		Setting{Name: "memory", Value: onOff(memoryOn(cfg))},
		Setting{Name: "snapshot", Value: onOff(cfg.Snapshot.SnapshotEnabled())},
		Setting{Name: "guidelines ref", Value: cfg.Review.GuidelinesRef},
		Setting{Name: "server listen", Value: cfg.Server.Listen},
		Setting{Name: "worker concurrency", Value: strconv.Itoa(cfg.Worker.Concurrency)},
	)
	return settings
}

// memoryOn mirrors the reviewer: memory needs both an embedding provider and a
// database, and either one alone leaves it off.
func memoryOn(cfg *config.Config) bool {
	if cfg.Database.DSN == "" {
		return false
	}
	provider, err := cfg.Models.EmbeddingProvider()
	return err == nil && provider != nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// redactor removes the configured secrets from everything doctor reports. The
// values are collected from the configuration rather than matched by shape,
// because a provider key is any string the operator's provider issued.
type redactor struct {
	secrets []string
}

func newRedactor(cfg *config.Config) *redactor {
	if cfg == nil {
		return &redactor{}
	}

	var secrets []string
	for _, provider := range cfg.Models.Providers {
		secrets = append(secrets, provider.APIKey)
	}
	secrets = append(secrets,
		cfg.Moderation.AdminToken,
		cfg.GitHub.WebhookSecret,
		cfg.GitHub.PrivateKeyPEM,
		cfg.Database.DSN,
		dsnPassword(cfg.Database.DSN),
	)

	// A shorter secret that is a substring of a longer one would leave the
	// longer one's tail visible, so the longest replacement goes first.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })

	kept := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		// A one- or two-character value redacts ordinary words and hides
		// nothing worth hiding; those are not credentials in any useful sense.
		if len(secret) >= 4 {
			kept = append(kept, secret)
		}
	}
	return &redactor{secrets: kept}
}

// text replaces every configured secret with a marker.
func (r *redactor) text(value string) string {
	for _, secret := range r.secrets {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

// dsnPassword extracts the password from a Postgres URL so an error that prints
// the DSN without it being the configured whole still hides the password.
func dsnPassword(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		return ""
	}
	password, _ := parsed.User.Password()
	return password
}
