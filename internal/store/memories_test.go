package store

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
)

const (
	testOrg  = "acme"
	testRepo = "payments"
)

// embeddingAt returns a vector that is orthogonal to every other embeddingAt,
// so two different indexes are unrelated rules and the same index is an exact
// match.
func embeddingAt(index int) []float32 {
	v := make([]float32, config.EmbeddingDimensions)
	v[index] = 1
	return v
}

// embeddingTilted returns a vector whose cosine similarity to embeddingAt(0) is
// cos, so the near-duplicate threshold can be probed from either side.
func embeddingTilted(cos float64) []float32 {
	v := make([]float32, config.EmbeddingDimensions)
	v[0] = float32(cos)
	v[1] = float32(math.Sqrt(1 - cos*cos))
	return v
}

func testProposal(ruleText string, embedding []float32) Proposal {
	return Proposal{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Scope:          ScopeRepo,
		RuleText:       ruleText,
		Rationale:      "the author confirmed this in review",
		PathGlob:       "pkg/auth/**",
		SourcePR:       42,
		SourceComment:  "https://github.com/acme/payments/pull/42#discussion_r1",
		RequestedBy:    "octocat",
		Embedding:      embedding,
	}
}

func countMemories(t *testing.T, db *DB) int {
	t.Helper()

	var count int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FROM memories`).Scan(&count); err != nil {
		t.Fatalf("counting memories: %v", err)
	}
	return count
}

func TestProposeMemoryStoresAProposal(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	memory, inserted, err := db.ProposeMemory(ctx, testProposal("Clock skew up to 60s is intentional", embeddingAt(0)))
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}
	if !inserted {
		t.Error("ProposeMemory reported no insert for a rule nothing matched")
	}
	if memory.Status != MemoryPending {
		t.Errorf("status = %s, want %s", memory.Status, MemoryPending)
	}
	if memory.Scope != ScopeRepo {
		t.Errorf("scope = %s, want %s", memory.Scope, ScopeRepo)
	}
	if memory.OrgName != testOrg || memory.RepoName != testRepo {
		t.Errorf("memory landed at %s/%s, want %s/%s", memory.OrgName, memory.RepoName, testOrg, testRepo)
	}
	if memory.PathGlob != "pkg/auth/**" {
		t.Errorf("path_glob = %q, want the glob the proposal carried", memory.PathGlob)
	}
	if memory.SourcePR != 42 || memory.RequestedBy != "octocat" || memory.SourceComment == "" {
		t.Errorf("the triggering comment left source_pr/requested_by/source_comment unset: %+v", memory)
	}
	if got := memory.ShortID(); len(got) != 8 || !strings.HasPrefix(memory.ID, got) {
		t.Errorf("ShortID() = %q, want the leading group of %s", got, memory.ID)
	}
}

// T-020-04: a second developer asking for the rule the first one asked for gets
// the existing memory back, and nothing is inserted.
func TestProposeMemorySkipsANearDuplicate(t *testing.T) {
	for _, status := range []string{"the first proposal", "an approved rule"} {
		t.Run(status, func(t *testing.T) {
			db := testDB(t)
			ctx := context.Background()

			first := testProposal("Allow 60s of clock skew in the JWT validator", embeddingAt(0))
			first.Approved = status == "an approved rule"
			stored, _, err := db.ProposeMemory(ctx, first)
			if err != nil {
				t.Fatalf("first ProposeMemory returned error: %v", err)
			}

			// The same rule worded differently lands at cosine 0.95.
			second := testProposal("A minute of clock skew is fine here", embeddingTilted(0.95))
			duplicate, inserted, err := db.ProposeMemory(ctx, second)
			if err != nil {
				t.Fatalf("second ProposeMemory returned error: %v", err)
			}
			if inserted {
				t.Error("the near-duplicate was stored as a second row")
			}
			if duplicate.ID != stored.ID {
				t.Errorf("answered with %s, want the existing memory %s", duplicate.ID, stored.ID)
			}
			if got := countMemories(t, db); got != 1 {
				t.Errorf("memories = %d, want 1", got)
			}
		})
	}
}

// A rule at cosine 0.88 is a different rule, not a worse wording of the same
// one, so it is stored.
func TestProposeMemoryStoresARuleBelowTheDuplicateThreshold(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, _, err := db.ProposeMemory(ctx, testProposal("Allow 60s of clock skew", embeddingAt(0))); err != nil {
		t.Fatalf("first ProposeMemory returned error: %v", err)
	}
	_, inserted, err := db.ProposeMemory(ctx, testProposal("Tables are singular", embeddingTilted(0.88)))
	if err != nil {
		t.Fatalf("second ProposeMemory returned error: %v", err)
	}
	if !inserted {
		t.Error("a rule at cosine 0.88 was treated as a duplicate")
	}
	if got := countMemories(t, db); got != 2 {
		t.Errorf("memories = %d, want 2", got)
	}
}

// The duplicate check is scoped to the tenant: another organization's identical
// rule is a different memory.
func TestProposeMemoryKeepsAnotherTenantsCopy(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, _, err := db.ProposeMemory(ctx, testProposal("Allow 60s of clock skew", embeddingAt(0))); err != nil {
		t.Fatalf("first ProposeMemory returned error: %v", err)
	}

	other := testProposal("Allow 60s of clock skew", embeddingAt(0))
	other.OrgName = "globex"
	if _, inserted, err := db.ProposeMemory(ctx, other); err != nil {
		t.Fatalf("second ProposeMemory returned error: %v", err)
	} else if !inserted {
		t.Error("an identical rule for another organization was treated as a duplicate")
	}
}

// T-020-06: a proposal that is still waiting for moderation is inert. Nothing
// retrieves it until the moderation surface flips its status.
func TestSearchMemoriesReturnsOnlyApproved(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pending, _, err := db.ProposeMemory(ctx, testProposal("Clock skew up to 60s is intentional", embeddingAt(0)))
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
		FilePath:       "pkg/auth/jwt.go",
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("search returned %d rules for a PENDING row, want none", len(matches))
	}

	// v0.0.5 adds the moderation surface that makes this transition; until then
	// the test is the one that moves the row.
	if _, err := db.pool.Exec(ctx, `UPDATE memories SET status = 'APPROVED' WHERE id = $1`, pending.ID); err != nil {
		t.Fatalf("approving the memory: %v", err)
	}

	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories after approval returned error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search returned %d rules after approval, want 1", len(matches))
	}
	if matches[0].ID != pending.ID || matches[0].RuleText != pending.RuleText {
		t.Errorf("matched %+v, want the approved rule %s", matches[0], pending.ID)
	}
}

// T-020-05: repository names are unique per owner, so a rule stored for
// acme/payments must not surface for globex/payments.
func TestSearchMemoriesSeparatesTenants(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	rule := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	rule.Approved = true
	if _, _, err := db.ProposeMemory(ctx, rule); err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        "globex",
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("globex/payments search returned %d of acme/payments' rules, want none", len(matches))
	}

	query.OrgName = testOrg
	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories for the owning tenant returned error: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("acme/payments search returned %d rules, want 1", len(matches))
	}
}

func TestSearchMemoriesOrdersClosestFirst(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for text, embedding := range map[string][]float32{
		"the closest rule": embeddingAt(0),
		"the middle rule":  embeddingTilted(0.5),
		"the faraway rule": embeddingAt(1),
	} {
		rule := testProposal(text, embedding)
		rule.PathGlob = ""
		rule.Approved = true
		if _, _, err := db.ProposeMemory(ctx, rule); err != nil {
			t.Fatalf("ProposeMemory(%s) returned error: %v", text, err)
		}
	}

	matches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("search returned %d rules, want 3", len(matches))
	}
	want := []string{"the closest rule", "the middle rule", "the faraway rule"}
	for i, text := range want {
		if matches[i].RuleText != text {
			t.Errorf("match %d is %q, want %q", i, matches[i].RuleText, text)
		}
	}
	if matches[0].Similarity <= matches[2].Similarity {
		t.Errorf("similarities are %v, want the first above the last", matches)
	}
}

// A glob narrows the candidates: a NULL glob applies to every path, and the
// search still fills its limit when the closest rule does not match the file.
func TestSearchMemoriesFiltersByGlob(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, rule := range []struct {
		text string
		glob string
		vec  []float32
	}{
		{"docs only", "docs/**", embeddingAt(0)},
		{"payments only", "internal/payments/**", embeddingAt(1)},
		{"everywhere", "", embeddingAt(2)},
	} {
		p := testProposal(rule.text, rule.vec)
		p.PathGlob = rule.glob
		p.Approved = true
		if _, _, err := db.ProposeMemory(ctx, p); err != nil {
			t.Fatalf("ProposeMemory(%s) returned error: %v", rule.text, err)
		}
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	}

	all, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("search without a path returned %d rules, want every candidate", len(all))
	}

	// The closest rule carries the docs glob, so a path under internal/payments
	// has to look past it to fill the limit of one.
	query.FilePath = "internal/payments/charge.go"
	query.Limit = 1
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories with a path returned error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search returned %d rules for internal/payments/charge.go, want 1", len(matches))
	}
	if matches[0].RuleText != "payments only" {
		t.Errorf("match is %q, want the rule whose glob covers the file", matches[0].RuleText)
	}
}

func TestSearchMemoriesClampsTheLimit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for i := 0; i < MemoryLimitMax+5; i++ {
		p := testProposal("rule "+string(rune('a'+i)), embeddingAt(i))
		p.PathGlob = ""
		p.Approved = true
		if _, _, err := db.ProposeMemory(ctx, p); err != nil {
			t.Fatalf("ProposeMemory returned error: %v", err)
		}
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	}
	for _, tc := range []struct {
		limit int
		want  int
	}{
		{99, MemoryLimitMax},
		{0, MemoryLimitDefault},
		{1, 1},
	} {
		query.Limit = tc.limit
		matches, err := db.SearchMemories(ctx, query)
		if err != nil {
			t.Fatalf("SearchMemories(limit %d) returned error: %v", tc.limit, err)
		}
		if len(matches) != tc.want {
			t.Errorf("limit %d returned %d rules, want %d", tc.limit, len(matches), tc.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"internal/payments/**", "internal/payments/charge.go", true},
		{"internal/payments/**", "internal/payments/nested/charge.go", true},
		{"internal/payments/**", "internal/billing/charge.go", false},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/*/main.go", "cmd/rumil/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"?at.go", "cat.go", true},
		{"?at.go", "chat.go", false},
		{"exact/path.go", "exact/path.go", true},
		{"exact/path.go", "exact/path.xgo", false},
		{"a.b/c+d.go", "a.b/c+d.go", true},
		{"a.b/c+d.go", "aXb/c+d.go", false},
	} {
		if got := globMatch(tc.glob, tc.path); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}
