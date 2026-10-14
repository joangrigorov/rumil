package mcpservers_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// fakeEmbedder hands every text a basis vector of its own, so two rules never
// look alike by accident and the same text always looks the same.
type fakeEmbedder struct {
	index map[string]int
	err   error
}

func newFakeEmbedder() *fakeEmbedder {
	return &fakeEmbedder{index: map[string]int{}}
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	index, ok := f.index[text]
	if !ok {
		index = len(f.index)
		f.index[text] = index
	}
	vector := make([]float32, config.EmbeddingDimensions)
	vector[index] = 1
	return vector, nil
}

func memoryLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// memoryTenant puts each test on a repository of its own, which is also the
// tenant the store scopes every query by.
func memoryTenant(repo string) mcpservers.MemorySession {
	return mcpservers.MemorySession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       repo,
		PRNumber:       42,
		SourceComment:  "https://github.com/acme/" + repo + "/pull/42#discussion_r1",
		RequestedBy:    "octocat",
	}
}

// testMemoryStore opens the database the memory tests share and empties it, so
// a rerun against the same local Postgres starts from nothing.
func testMemoryStore(t *testing.T) *store.DB {
	t.Helper()

	dsn := testutil.TestDatabaseURL(t, "rumil_mcpservers_test")
	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to empty the memory table: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE memories`); err != nil {
		t.Fatalf("truncating memories: %v", err)
	}
	return db
}

func callTool(t *testing.T, specs []mcpservers.Spec, name string, args map[string]any) (string, error) {
	t.Helper()

	for _, spec := range specs {
		if spec.Name == name {
			return spec.Handle(context.Background(), args)
		}
	}
	t.Fatalf("the tool set has no %s", name)
	return "", nil
}

func mustCall(t *testing.T, specs []mcpservers.Spec, name string, args map[string]any) string {
	t.Helper()

	out, err := callTool(t, specs, name, args)
	if err != nil {
		t.Fatalf("%s returned error: %v", name, err)
	}
	return out
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()

	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decoding the tool result %q: %v", raw, err)
	}
	return out
}

type searchResult struct {
	Rules []struct {
		ID         string  `json:"id"`
		Scope      string  `json:"scope"`
		Repo       string  `json:"repo"`
		PathGlob   string  `json:"path_glob"`
		Rule       string  `json:"rule"`
		Rationale  string  `json:"rationale"`
		Similarity float64 `json:"similarity"`
	} `json:"rules"`
	Count int `json:"count"`
}

type proposeResult struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Status  string `json:"status"`
	Scope   string `json:"scope"`
	Repo    string `json:"repo"`
	Created bool   `json:"created"`
	Note    string `json:"note"`
}

// TestMemorySpecsPinTheToolSurface pins what the model sees, and that the two
// halves of the surface stay apart: the session that reads rules cannot write
// one, and the session that writes one cannot read.
func TestMemorySpecsPinTheToolSurface(t *testing.T) {
	tools := mcpservers.NewMemoryTools(nil, nil, mcpservers.MemorySession{}, memoryLogger())

	search := tools.SearchSpecs()
	if len(search) != 1 || search[0].Name != "search_memory" {
		t.Fatalf("search tool set is %+v, want only search_memory", search)
	}
	propose := tools.ProposeSpecs()
	if len(propose) != 1 || propose[0].Name != "propose_memory" {
		t.Fatalf("propose tool set is %+v, want only propose_memory", propose)
	}

	for _, spec := range append(append([]mcpservers.Spec{}, search...), propose...) {
		if spec.Description == "" {
			t.Errorf("%s has no description", spec.Name)
		}
		if spec.Handle == nil {
			t.Errorf("%s has no handler", spec.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(spec.Schema, &schema); err != nil {
			t.Errorf("%s has an undecodable schema: %v", spec.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", spec.Name, schema["type"])
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s schema does not forbid additional properties", spec.Name)
		}
	}

	searchSchema := string(search[0].Schema)
	for _, want := range []string{"query", "file_path", "limit", `"required":["query"]`} {
		if !strings.Contains(searchSchema, want) {
			t.Errorf("search_memory schema lost %s: %s", want, searchSchema)
		}
	}
	proposeSchema := string(propose[0].Schema)
	for _, want := range []string{"rule_text", "rationale", "path_glob", "scope", "REPO", "ORG", `"required":["rule_text","rationale"]`} {
		if !strings.Contains(proposeSchema, want) {
			t.Errorf("propose_memory schema lost %s: %s", want, proposeSchema)
		}
	}
}

func TestProposeMemoryFillsTheColumnsTheModelDoesNotSupply(t *testing.T) {
	db := testMemoryStore(t)
	embedder := newFakeEmbedder()
	tools := mcpservers.NewMemoryTools(db, embedder, memoryTenant("propose"), memoryLogger())

	var stored store.Memory
	var inserted bool
	tools.OnMemoryProposed = func(memory store.Memory, created bool) {
		stored, inserted = memory, created
	}

	out := decode[proposeResult](t, mustCall(t, tools.ProposeSpecs(), "propose_memory", map[string]any{
		"rule_text": "Clock skew of up to 60 seconds is intentional in the JWT validator",
		"rationale": "the author confirmed it in review",
		"path_glob": "pkg/auth/**",
	}))

	if !inserted || !out.Created {
		t.Error("a new rule was not reported as created")
	}
	if out.Status != store.MemoryPending {
		t.Errorf("status = %s, want %s with moderation off the default", out.Status, store.MemoryPending)
	}
	if out.Scope != store.ScopeRepo {
		t.Errorf("scope = %s, want %s", out.Scope, store.ScopeRepo)
	}
	if out.Repo != "acme/propose" {
		t.Errorf("repo = %s, want the session's repository", out.Repo)
	}
	if out.ShortID == "" || !strings.HasPrefix(out.ID, out.ShortID) {
		t.Errorf("short_id %q is not the leading group of %q", out.ShortID, out.ID)
	}

	if stored.SourcePR != 42 || stored.RequestedBy != "octocat" || stored.SourceComment == "" {
		t.Errorf("the triggering comment was not recorded: %+v", stored)
	}
	if stored.PathGlob != "pkg/auth/**" {
		t.Errorf("path_glob = %q, want the glob the model passed", stored.PathGlob)
	}
}

// An operator who turned moderation off has accepted that comment text reaches
// the review prompt unvetted, so the row lands APPROVED.
func TestProposeMemoryApprovesWhenAcceptAllIsOn(t *testing.T) {
	db := testMemoryStore(t)
	session := memoryTenant("accept-all")
	session.AcceptAll = true
	tools := mcpservers.NewMemoryTools(db, newFakeEmbedder(), session, memoryLogger())

	out := decode[proposeResult](t, mustCall(t, tools.ProposeSpecs(), "propose_memory", map[string]any{
		"rule_text": "Every handler is a single-statement upsert, so no explicit transaction is needed",
		"rationale": "the author confirmed it in review",
	}))

	if out.Status != store.MemoryApproved {
		t.Errorf("status = %s, want %s when accept_all is on", out.Status, store.MemoryApproved)
	}
}

// T-020-06 (second half): the chain "propose with accept_all on, retrieve it"
// runs as one flow, so the retrieval half is proven against a row the gate let
// through rather than against one a test wrote APPROVED itself.
func TestAcceptAllProposalIsRetrievable(t *testing.T) {
	db := testMemoryStore(t)
	embedder := newFakeEmbedder()
	session := memoryTenant("accept-all-recall")
	session.AcceptAll = true
	const rule = "Clock skew of up to 60 seconds is intentional in the JWT validator"

	writing := mcpservers.NewMemoryTools(db, embedder, session, memoryLogger())
	proposed := decode[proposeResult](t, mustCall(t, writing.ProposeSpecs(), "propose_memory", map[string]any{
		"rule_text": rule,
		"rationale": "the author confirmed it in review",
		"path_glob": "pkg/auth/**",
	}))
	if proposed.Status != store.MemoryApproved {
		t.Fatalf("status = %s, want %s with accept_all on", proposed.Status, store.MemoryApproved)
	}

	reading := mcpservers.NewMemoryTools(db, embedder, session, memoryLogger())
	out := decode[searchResult](t, mustCall(t, reading.SearchSpecs(), "search_memory", map[string]any{
		"query":     rule,
		"file_path": "pkg/auth/jwt.go",
	}))
	if out.Count != 1 || out.Rules[0].ID != proposed.ID {
		t.Errorf("search returned %+v, want the rule accept_all approved", out.Rules)
	}
}

// T-020-04: the second developer asking for the same rule is answered with the
// memory the first one produced.
func TestProposeMemoryAnswersADuplicateWithTheExistingMemory(t *testing.T) {
	db := testMemoryStore(t)
	tools := mcpservers.NewMemoryTools(db, newFakeEmbedder(), memoryTenant("duplicate"), memoryLogger())

	args := map[string]any{
		"rule_text": "Clock skew of up to 60 seconds is intentional",
		"rationale": "the author confirmed it in review",
	}
	first := decode[proposeResult](t, mustCall(t, tools.ProposeSpecs(), "propose_memory", args))

	second := decode[proposeResult](t, mustCall(t, tools.ProposeSpecs(), "propose_memory", args))
	if second.Created {
		t.Error("the duplicate was reported as created")
	}
	if second.ID != first.ID || second.ShortID != first.ShortID {
		t.Errorf("duplicate answered with %s, want %s", second.ID, first.ID)
	}
	if !strings.Contains(second.Note, first.ShortID) {
		t.Errorf("note %q does not name the existing memory", second.Note)
	}
}

// T-020-06: a rule nobody has approved yet never reaches the agent, whatever
// its glob says about the file under review.
func TestSearchMemoryKeepsPendingRulesAway(t *testing.T) {
	db := testMemoryStore(t)
	embedder := newFakeEmbedder()
	session := memoryTenant("pending")
	const rule = "Clock skew of up to 60 seconds is intentional"

	writing := mcpservers.NewMemoryTools(db, embedder, session, memoryLogger())
	mustCall(t, writing.ProposeSpecs(), "propose_memory", map[string]any{
		"rule_text": rule,
		"rationale": "the author confirmed it in review",
		"path_glob": "pkg/auth/**",
	})

	reading := mcpservers.NewMemoryTools(db, embedder, session, memoryLogger())
	out := decode[searchResult](t, mustCall(t, reading.SearchSpecs(), "search_memory", map[string]any{
		"query":     "clock skew validation",
		"file_path": "pkg/auth/jwt.go",
		"limit":     json.Number("5"),
	}))
	if out.Count != 0 || len(out.Rules) != 0 {
		t.Fatalf("search returned %+v for a PENDING rule, want nothing", out.Rules)
	}
}

func TestSearchMemoryReturnsApprovedRules(t *testing.T) {
	db := testMemoryStore(t)
	ctx := context.Background()
	embedder := newFakeEmbedder()
	session := memoryTenant("approved")

	const rule = "Clock skew of up to 60 seconds is intentional in the JWT validator"
	embedding, err := embedder.Embed(ctx, rule)
	if err != nil {
		t.Fatalf("embedding the rule: %v", err)
	}
	proposed, _, err := db.ProposeMemory(ctx, store.Proposal{
		InstallationID: session.InstallationID,
		OrgName:        session.OrgName,
		RepoName:       session.RepoName,
		Scope:          store.ScopeRepo,
		RuleText:       rule,
		Rationale:      "the author confirmed it in review",
		PathGlob:       "pkg/auth/**",
		SourcePR:       session.PRNumber,
		SourceComment:  session.SourceComment,
		RequestedBy:    session.RequestedBy,
		Approved:       true,
		Embedding:      embedding,
	})
	if err != nil {
		t.Fatalf("seeding the approved rule: %v", err)
	}

	tools := mcpservers.NewMemoryTools(db, embedder, session, memoryLogger())
	out := decode[searchResult](t, mustCall(t, tools.SearchSpecs(), "search_memory", map[string]any{
		"query":     rule,
		"file_path": "pkg/auth/jwt.go",
	}))

	if out.Count != 1 {
		t.Fatalf("search returned %d rules, want 1: %+v", out.Count, out.Rules)
	}
	found := out.Rules[0]
	if found.ID != proposed.ID || found.Rule != rule {
		t.Errorf("matched %+v, want the seeded rule %s", found, proposed.ID)
	}
	if found.Scope != store.ScopeRepo || found.Repo != "acme/approved" {
		t.Errorf("match reports scope %s repo %s, want REPO acme/approved", found.Scope, found.Repo)
	}
	if found.Similarity < 0.99 {
		t.Errorf("similarity = %v, want an exact match for the same text", found.Similarity)
	}
}

func TestSearchMemoryKeepsOtherTenantsRulesAway(t *testing.T) {
	db := testMemoryStore(t)
	ctx := context.Background()
	embedder := newFakeEmbedder()

	const rule = "Clock skew of up to 60 seconds is intentional"
	embedding, err := embedder.Embed(ctx, rule)
	if err != nil {
		t.Fatalf("embedding the rule: %v", err)
	}
	other := memoryTenant("payments")
	other.OrgName = "globex"
	if _, _, err := db.ProposeMemory(ctx, store.Proposal{
		InstallationID: other.InstallationID,
		OrgName:        other.OrgName,
		RepoName:       other.RepoName,
		Scope:          store.ScopeRepo,
		RuleText:       rule,
		Rationale:      "recorded for another organization",
		SourcePR:       other.PRNumber,
		SourceComment:  other.SourceComment,
		RequestedBy:    other.RequestedBy,
		Approved:       true,
		Embedding:      embedding,
	}); err != nil {
		t.Fatalf("seeding the other tenant's rule: %v", err)
	}

	tools := mcpservers.NewMemoryTools(db, embedder, memoryTenant("payments"), memoryLogger())
	out := decode[searchResult](t, mustCall(t, tools.SearchSpecs(), "search_memory", map[string]any{
		"query": rule,
	}))
	if out.Count != 0 {
		t.Errorf("search returned %d of globex/payments' rules, want none: %+v", out.Count, out.Rules)
	}
}

// Organization-wide rules arrive with v0.0.6. Until then the row is the
// repository's, and the result says so.
func TestProposeMemoryRecordsRepoScopeWhenOrgIsAsked(t *testing.T) {
	db := testMemoryStore(t)
	tools := mcpservers.NewMemoryTools(db, newFakeEmbedder(), memoryTenant("org-scope"), memoryLogger())

	out := decode[proposeResult](t, mustCall(t, tools.ProposeSpecs(), "propose_memory", map[string]any{
		"rule_text": "Migrations are always additive",
		"rationale": "the team agreed in review",
		"scope":     "ORG",
	}))
	if out.Scope != store.ScopeRepo {
		t.Errorf("scope = %s, want %s until org memory ships", out.Scope, store.ScopeRepo)
	}
	if !strings.Contains(out.Note, "not available yet") {
		t.Errorf("note = %q, want it to say the rule was recorded for the repository", out.Note)
	}
}

func TestMemoryToolsRejectIncompleteCalls(t *testing.T) {
	db := testMemoryStore(t)
	tools := mcpservers.NewMemoryTools(db, newFakeEmbedder(), memoryTenant("validation"), memoryLogger())

	for _, tc := range []struct {
		name string
		tool string
		spec []mcpservers.Spec
		args map[string]any
		want string
	}{
		{"search without a query", "search_memory", tools.SearchSpecs(), map[string]any{}, "query is required"},
		{"propose without a rule", "propose_memory", tools.ProposeSpecs(), map[string]any{"rationale": "why"}, "rule_text is required"},
		{"propose without a rationale", "propose_memory", tools.ProposeSpecs(), map[string]any{"rule_text": "a rule"}, "rationale is required"},
		{"propose with an unknown scope", "propose_memory", tools.ProposeSpecs(), map[string]any{"rule_text": "a rule", "rationale": "why", "scope": "GLOBAL"}, "scope must be REPO or ORG"},
	} {
		_, err := callTool(t, tc.spec, tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s returned %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestSearchMemoryReportsAnEmbedderFailure(t *testing.T) {
	db := testMemoryStore(t)
	embedder := newFakeEmbedder()
	embedder.err = context.DeadlineExceeded
	tools := mcpservers.NewMemoryTools(db, embedder, memoryTenant("embedder-down"), memoryLogger())

	if _, err := callTool(t, tools.SearchSpecs(), "search_memory", map[string]any{"query": "anything"}); err == nil {
		t.Error("search_memory hid an embedder failure")
	}
	if _, err := callTool(t, tools.ProposeSpecs(), "propose_memory", map[string]any{"rule_text": "a rule", "rationale": "why"}); err == nil {
		t.Error("propose_memory hid an embedder failure")
	}
}
