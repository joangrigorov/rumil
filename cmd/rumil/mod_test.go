package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// modConfig writes a configuration for the moderation commands: the test
// database, and an embedding provider pointed at the fake endpoint.
func modConfig(t *testing.T, dsn, llmURL string) string {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test private key: %v", err)
	}

	doc := fmt.Sprintf(`
app:
  max_agent_turns: 5
github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: %q
models:
  default_embedding: "embed"
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-model"
      api_key: "test-key"
    - id: "embed"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-embedding"
      api_key: "embed-key"
      dimensions: 1536
database:
  dsn: %q
  auto_migrate: true
`, keyPath, llmURL+"/v1", llmURL+"/v1", dsn)

	configPath := filepath.Join(t.TempDir(), "rumil.yaml")
	if err := os.WriteFile(configPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the test config: %v", err)
	}
	return configPath
}

// modDatabase returns the DSN the moderation tests share, emptied first so each
// test lists only what it seeded.
func modDatabase(t *testing.T) string {
	t.Helper()

	dsn := testutil.TestDatabaseURL(t, "rumil_cmd_test")
	ctx := context.Background()

	db, err := store.Open(ctx, dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	db.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to empty the memory table: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE memories`); err != nil {
		t.Fatalf("truncating memories: %v", err)
	}
	return dsn
}

// ruleVector spreads a rule's text over the dimensions, so two seeded rules
// never look like near-duplicates of each other: the store's duplicate check
// would otherwise collapse the second one into the first.
func ruleVector(rule string) []float32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(rule))
	return testutil.BasisVector(int(hash.Sum32() % testutil.EmbeddingDimensions))
}

// seedModerationRule records a pending repository rule.
func seedModerationRule(t *testing.T, dsn, rule string) store.Memory {
	t.Helper()
	return seedScopedModerationRule(t, dsn, rule, store.ScopeRepo)
}

// seedScopedModerationRule records a pending rule in one of the two scopes,
// with the repository it was proposed from on the row either way.
func seedScopedModerationRule(t *testing.T, dsn, rule, scope string) store.Memory {
	t.Helper()

	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	memory, _, err := db.ProposeMemory(ctx, store.Proposal{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		Scope:          scope,
		RuleText:       rule,
		Rationale:      "the author confirmed it in review",
		SourcePR:       42,
		SourceComment:  "https://github.com/acme/payments/pull/42#discussion_r1",
		RequestedBy:    "octocat",
		Embedding:      ruleVector(rule),
	})
	if err != nil {
		t.Fatalf("seeding a rule: %v", err)
	}
	return memory
}

func readModerationRule(t *testing.T, dsn, id string) store.Memory {
	t.Helper()

	db, err := store.Open(context.Background(), dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	memory, err := db.MemoryByID(context.Background(), 98765432, id)
	if err != nil {
		t.Fatalf("reading the rule back: %v", err)
	}
	return memory
}

// runModCmd runs one moderation command through the real CLI.
func runModCmd(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(append([]string{"mod", "--config", configPath}, args...))

	err := root.Execute()
	return out.String(), err
}

// modColumn reads one column of the first rule a `mod list` printed, by the
// position its header names it in. A value is checked where the table put it,
// so a column that printed another column's value is caught.
//
// The cells are split on the runs of two or more spaces tabwriter pads with,
// because a cell of its own can hold a single space (an organization rule's
// "acme (proposed in payments)", for one).
func modColumn(t *testing.T, listing, header string) string {
	t.Helper()

	rows := strings.Split(strings.TrimRight(listing, "\n"), "\n")
	if len(rows) < 2 {
		t.Fatalf("the listing has no rule to read a %s from:\n%s", header, listing)
	}
	cells := func(row string) []string {
		return tabwriterCell.Split(strings.TrimSpace(row), -1)
	}
	names, values := cells(rows[0]), cells(rows[1])
	for i, name := range names {
		if name == header {
			if i >= len(values) {
				t.Fatalf("the rule row is short of the %s column: %q", header, rows[1])
			}
			return values[i]
		}
	}
	t.Fatalf("the header does not carry a %s column: %q", header, rows[0])
	return ""
}

// tabwriterCell is the boundary between two columns of a tabwriter table.
var tabwriterCell = regexp.MustCompile(`\s{2,}`)

// The list shows what is waiting, and nothing a maintainer has already decided.
func TestModListShowsWhatIsWaiting(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	waiting := seedModerationRule(t, dsn, "Clock skew up to 60 seconds is intentional")
	decided := seedModerationRule(t, dsn, "Settlements are retried with a bounded budget")

	// The second rule is approved through the CLI, which is also the T-030-02
	// path: the row transitions and records who decided.
	out, err := runModCmd(t, configPath, "approve", decided.ID[:8], "--moderator", "octocat")
	if err != nil {
		t.Fatalf("approve returned error: %v (output: %s)", err, out)
	}
	approved := readModerationRule(t, dsn, decided.ID)
	if approved.Status != store.MemoryApproved || approved.ReviewedBy != "octocat" {
		t.Fatalf("row is %+v, want APPROVED and reviewed_by octocat", approved)
	}

	// The list is the whole queue unless it is filtered.
	all, err := runModCmd(t, configPath, "list")
	if err != nil {
		t.Fatalf("list returned error: %v (output: %s)", err, all)
	}
	for _, want := range []string{"PENDING", "APPROVED", "Clock skew", "Settlements", waiting.ID[:8]} {
		if !strings.Contains(all, want) {
			t.Errorf("the list lost %q:\n%s", want, all)
		}
	}

	pending, err := runModCmd(t, configPath, "list", "--status", "pending")
	if err != nil {
		t.Fatalf("list --status pending returned error: %v (output: %s)", err, pending)
	}
	if !strings.Contains(pending, "Clock skew") {
		t.Errorf("the pending list lost the waiting rule:\n%s", pending)
	}
	if strings.Contains(pending, "Settlements") {
		t.Errorf("the pending list carries a decided rule:\n%s", pending)
	}

	byRepo, err := runModCmd(t, configPath, "list", "--repo", "acme/payments")
	if err != nil {
		t.Fatalf("list --repo returned error: %v (output: %s)", err, byRepo)
	}
	if !strings.Contains(byRepo, "Clock skew") {
		t.Errorf("the repository filter lost the rule:\n%s", byRepo)
	}
	if other, err := runModCmd(t, configPath, "list", "--repo", "globex/payments"); err != nil {
		t.Fatalf("list --repo returned error: %v (output: %s)", err, other)
	} else if !strings.Contains(other, "no rules match") {
		t.Errorf("another tenant's list is not empty:\n%s", other)
	}
}

// T-030-03 (CLI half): an edited wording is re-embedded through the configured
// provider before the row is approved, so the rule is found by the words the
// moderator approved.
func TestModApproveReEmbedsAnEditedRule(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	const proposed = "Clock skew up to 60 seconds is intentional"
	const edited = "Clock skew up to 60 seconds is intentional in the JWT validator only"
	memory := seedModerationRule(t, dsn, proposed)

	out, err := runModCmd(t, configPath, "approve", memory.ID[:8], "--rule-text", edited)
	if err != nil {
		t.Fatalf("approve returned error: %v (output: %s)", err, out)
	}

	stored := readModerationRule(t, dsn, memory.ID)
	if stored.RuleText != edited {
		t.Errorf("rule_text = %q, want the edited wording", stored.RuleText)
	}
	if stored.Status != store.MemoryApproved {
		t.Errorf("status = %s, want %s", stored.Status, store.MemoryApproved)
	}
	if texts := llm.EmbeddedTexts(); len(texts) != 1 || texts[0] != edited {
		t.Errorf("embedded %v, want exactly the edited wording", texts)
	}
}

// T-040-04 (CLI half): the list says which scope a rule has and what an
// organization rule reaches, and --scope narrows one to the repository it was
// proposed from before it is approved.
func TestModScopesAnOrganizationRule(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	org := seedScopedModerationRule(t, dsn, "All Go services in the organization log with log/slog", store.ScopeOrg)
	repo := seedModerationRule(t, dsn, "Settlements are retried with a bounded budget")

	out, err := runModCmd(t, configPath, "list")
	if err != nil {
		t.Fatalf("list returned error: %v (output: %s)", err, out)
	}
	header, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(header, "SCOPE") {
		t.Errorf("the list header does not name the scope column: %q", header)
	}
	if !strings.Contains(out, "acme (proposed in payments)") {
		t.Errorf("an organization rule is not shown as reaching the organization:\n%s", out)
	}
	if !strings.Contains(out, "acme/payments") {
		t.Errorf("a repository rule is not shown as reaching its repository:\n%s", out)
	}

	out, err = runModCmd(t, configPath, "approve", org.ID[:8], "--scope", "REPO")
	if err != nil {
		t.Fatalf("approve --scope returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, store.ScopeRepo) {
		t.Errorf("the answer does not name the scope it approved with: %s", out)
	}
	stored := readModerationRule(t, dsn, org.ID)
	if stored.Scope != store.ScopeRepo || stored.Status != store.MemoryApproved {
		t.Errorf("row is %+v, want an approved %s rule", stored, store.ScopeRepo)
	}
	if stored.RepoName != "payments" {
		t.Errorf("repo_name = %q, want the repository it was proposed from", stored.RepoName)
	}

	// A scope that is not one a rule can have is answered with the two that are.
	if out, err = runModCmd(t, configPath, "approve", repo.ID[:8], "--scope", "GLOBAL"); err == nil {
		t.Fatalf("an unknown scope was accepted: %s", out)
	} else if !strings.Contains(err.Error(), "scope takes REPO or ORG") {
		t.Errorf("error = %v, want the two scopes", err)
	}

	// Narrowing is the only direction: a repository rule is not widened to the
	// whole organization from a moderation decision.
	if out, err = runModCmd(t, configPath, "approve", repo.ID[:8], "--scope", "ORG"); err == nil {
		t.Fatalf("a repository rule was widened to the organization: %s", out)
	} else if !strings.Contains(err.Error(), "is not widened") {
		t.Errorf("error = %v, want the narrowing rule stated", err)
	}
	if stored := readModerationRule(t, dsn, repo.ID); stored.Scope != store.ScopeRepo || stored.Status != store.MemoryPending {
		t.Errorf("a refused widening changed the row: %+v", stored)
	}
}

// T-030-04 (CLI half): a rejected rule stays on record and is not retrievable.
func TestModRejectHidesARule(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())
	memory := seedModerationRule(t, dsn, "Settlements are retried with a bounded budget")

	out, err := runModCmd(t, configPath, "reject", memory.ID[:8], "--moderator", "octocat")
	if err != nil {
		t.Fatalf("reject returned error: %v (output: %s)", err, out)
	}
	rejected := readModerationRule(t, dsn, memory.ID)
	if rejected.Status != store.MemoryRejected || rejected.ReviewedBy != "octocat" {
		t.Errorf("row is %+v, want REJECTED and reviewed_by octocat", rejected)
	}

	db, err := store.Open(context.Background(), dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()
	matches, err := db.SearchMemories(context.Background(), store.MemoryQuery{
		InstallationID: 98765432, OrgName: "acme", RepoName: "payments",
		Embedding: ruleVector(rejected.RuleText), Limit: store.MemoryLimitMax,
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	for _, match := range matches {
		if match.ID == memory.ID {
			t.Errorf("a rejected rule is retrievable: %+v", match)
		}
	}
}

// A moderation command names one rule, and says so when there is no such rule.
func TestModDecisionsNeedAnExistingRule(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	out, err := runModCmd(t, configPath, "approve", "00000000")
	if err == nil {
		t.Fatalf("approving an unknown rule succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "memory not found") {
		t.Errorf("error = %v, want the not-found reason", err)
	}

	if out, err := runModCmd(t, configPath, "list", "--repo", "payments"); err == nil {
		t.Errorf("a repository that is not owner/name was accepted: %s", out)
	}

	// A status the enum does not have is the operator's typo, and it is answered
	// with what the statuses are rather than with a database error.
	out, err = runModCmd(t, configPath, "list", "--status", "approve")
	if err == nil {
		t.Fatalf("an unknown status was accepted: %s", out)
	}
	if !strings.Contains(err.Error(), "pending, approved or rejected") {
		t.Errorf("error = %v, want the three statuses", err)
	}
}

// T-030-05 (warning half): turning the approval gate off is reported on every
// start, and so is a moderation surface with no token behind it.
func TestWarnAboutModeration(t *testing.T) {
	cases := []struct {
		name     string
		cfg      config.Config
		serve    bool
		want     []string
		unwanted []string
	}{
		{
			name: "the gate is off",
			cfg:  config.Config{Moderation: config.Moderation{AcceptAll: true}},
			want: []string{"accept_all is on"},
		},
		{
			name:     "the gate is on",
			cfg:      config.Config{},
			unwanted: []string{"accept_all is on"},
		},
		{
			name:  "serve with no admin token",
			cfg:   config.Config{},
			serve: true,
			want:  []string{"admin_token is not set"},
		},
		{
			name:     "a moderation command with no admin token",
			cfg:      config.Config{},
			unwanted: []string{"admin_token is not set"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			warnAboutModeration(&tc.cfg, slog.New(slog.NewJSONHandler(logs, nil)), tc.serve)

			for _, want := range tc.want {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs lost %q:\n%s", want, logs.String())
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(logs.String(), unwanted) {
					t.Errorf("logs carry %q:\n%s", unwanted, logs.String())
				}
			}
		})
	}
}

// §10 item 5 (CLI half): the approval gate is where a rule is raised above the
// repository's own guidelines, the list shows which rules carry that authority,
// and the promotion is offered for one repository only.
func TestModPromotesAboveTheGuidelines(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	repo := seedModerationRule(t, dsn, "This service keeps its own hand-rolled clock skew allowance")
	org := seedScopedModerationRule(t, dsn, "Every service in the organization logs with log/slog", store.ScopeOrg)

	out, err := runModCmd(t, configPath, "list")
	if err != nil {
		t.Fatalf("list returned error: %v (output: %s)", err, out)
	}
	header, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(header, "AUTHORITY") {
		t.Errorf("the list header does not name the authority column: %q", header)
	}
	// The column a value lands in is what the header promises, so both are
	// checked by position rather than by a substring that any other column could
	// have supplied.
	if got := modColumn(t, out, "AUTHORITY"); got != store.AuthorityContext {
		t.Errorf("the AUTHORITY column reads %q for a rule nobody promoted, want %q", got, store.AuthorityContext)
	}

	// An authority that is not one a rule can have is answered with the two that
	// are.
	if out, err = runModCmd(t, configPath, "approve", repo.ID[:8], "--authority", "ABSOLUTE"); err == nil {
		t.Fatalf("an unknown authority was accepted: %s", out)
	} else if !errors.Is(err, moderation.ErrUnknownAuthority) || !strings.Contains(err.Error(), "authority takes context or directive") {
		t.Errorf("error = %v, want %v naming the two authorities", err, moderation.ErrUnknownAuthority)
	}

	out, err = runModCmd(t, configPath, "approve", repo.ID[:8], "--authority", "directive", "--moderator", "octocat")
	if err != nil {
		t.Fatalf("approve --authority returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, store.AuthorityDirective) {
		t.Errorf("the answer does not name the authority it approved with: %s", out)
	}
	stored := readModerationRule(t, dsn, repo.ID)
	if stored.Authority != store.AuthorityDirective || stored.Status != store.MemoryApproved {
		t.Errorf("row is %+v, want an approved %s rule", stored, store.AuthorityDirective)
	}
	if listing, err := runModCmd(t, configPath, "list", "--status", "approved"); err != nil {
		t.Fatalf("list --status returned error: %v", err)
	} else if got := modColumn(t, listing, "AUTHORITY"); got != store.AuthorityDirective {
		t.Errorf("the AUTHORITY column reads %q for the promoted rule, want %q", got, store.AuthorityDirective)
	}

	// A promotion is offered for a repository rule: one that still reaches the
	// organization is refused until it is narrowed.
	if out, err = runModCmd(t, configPath, "approve", org.ID[:8], "--authority", "DIRECTIVE"); err == nil {
		t.Fatalf("an organization-wide rule was promoted: %s", out)
	} else if !errors.Is(err, moderation.ErrAuthorityWidened) || !strings.Contains(err.Error(), "a directive applies to one repository") {
		t.Errorf("error = %v, want %v naming the one-repository rule", err, moderation.ErrAuthorityWidened)
	}
	if stored := readModerationRule(t, dsn, org.ID); stored.Authority != store.AuthorityContext || stored.Status != store.MemoryPending {
		t.Errorf("a refused promotion changed the row: %+v", stored)
	}

	// Narrowing it in the same command is the decision that lands.
	if out, err = runModCmd(t, configPath, "approve", org.ID[:8], "--scope", "REPO", "--authority", "DIRECTIVE"); err != nil {
		t.Fatalf("approve --scope REPO --authority returned error: %v (output: %s)", err, out)
	}
	stored = readModerationRule(t, dsn, org.ID)
	if stored.Scope != store.ScopeRepo || stored.Authority != store.AuthorityDirective {
		t.Errorf("row is %+v, want a promoted %s rule", stored, store.ScopeRepo)
	}
}
