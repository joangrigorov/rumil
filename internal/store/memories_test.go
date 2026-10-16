package store

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const (
	testOrg  = "acme"
	testRepo = "payments"
)

// embeddingAt returns a vector that is orthogonal to every other embeddingAt,
// so two different indexes are unrelated rules and the same index is an exact
// match.
func embeddingAt(index int) []float32 {
	v := make([]float32, testutil.EmbeddingDimensions)
	v[index] = 1
	return v
}

// embeddingTilted returns a vector whose cosine similarity to embeddingAt(0) is
// cos, so the near-duplicate threshold can be probed from either side.
func embeddingTilted(cos float64) []float32 {
	return embeddingLike(cos, 1)
}

// embeddingLike returns a unit vector at cosine cos from embeddingAt(0),
// leaning on axis. Two of them on different axes are far from each other
// however close each is to the query, which is what a ranking test needs: two
// rules that both sit at a chosen distance from the query without being
// near-duplicates of one another.
func embeddingLike(cos float64, axis int) []float32 {
	v := make([]float32, testutil.EmbeddingDimensions)
	v[0] = float32(cos)
	v[axis] = float32(math.Sqrt(1 - cos*cos))
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

// A repository proposal is compared against everything a review of that
// repository would already retrieve, the organization's rules included: the
// same rule asked for twice, once for the organization and once for a
// repository, is one rule rather than two bullets in the same prompt.
func TestProposeMemoryDoesNotRepeatAnOrganizationRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	org := testProposal("All services in the organization log with log/slog", embeddingAt(0))
	org.Scope, org.Approved = ScopeOrg, true
	stored, _, err := db.ProposeMemory(ctx, org)
	if err != nil {
		t.Fatalf("ProposeMemory(organization rule) returned error: %v", err)
	}

	// The same rule, this time asked for in a repository, worded differently
	// and at cosine 0.95.
	repo := testProposal("Services log with log/slog in this organization", embeddingTilted(0.95))
	repo.RepoName = "inventory-service"
	duplicate, inserted, err := db.ProposeMemory(ctx, repo)
	if err != nil {
		t.Fatalf("ProposeMemory(repository proposal) returned error: %v", err)
	}
	if inserted {
		t.Error("the organization's rule was stored a second time for the repository")
	}
	if duplicate.ID != stored.ID {
		t.Errorf("answered with %s, want the organization's rule %s", duplicate.ID, stored.ID)
	}
	if got := countMemories(t, db); got != 1 {
		t.Errorf("memories = %d, want 1", got)
	}

	// The other direction is a widening, which is a rule to record rather than
	// a duplicate to report: a repository's rule says nothing about the rest of
	// the organization.
	local := testProposal("Payments buffers settlement writes for a second", embeddingAt(1))
	local.Approved = true
	if _, _, err := db.ProposeMemory(ctx, local); err != nil {
		t.Fatalf("ProposeMemory(repository rule) returned error: %v", err)
	}
	widened := testProposal("Payments buffers settlement writes for a second", embeddingAt(1))
	widened.Scope = ScopeOrg
	if _, inserted, err := db.ProposeMemory(ctx, widened); err != nil {
		t.Fatalf("ProposeMemory(organization proposal) returned error: %v", err)
	} else if !inserted {
		t.Error("asking for the organization's own rule was answered with the repository's")
	}
	if got := countMemories(t, db); got != 3 {
		t.Errorf("memories = %d, want 3", got)
	}
}

// T-040-03: an organization rule is the organization's. Another organization's
// query returns nothing for it, and the same query from a second repository of
// the owning organization returns it, so the absence is the filter rather than
// a rule that never matched.
func TestT040_03_OrgRulesStayInTheirOrganization(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	org := testProposal("All services in the organization log with log/slog", embeddingAt(0))
	org.Scope, org.PathGlob, org.Approved = ScopeOrg, "", true
	stored, _, err := db.ProposeMemory(ctx, org)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        "globex",
		RepoName:       "api",
		Embedding:      embeddingAt(0),
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("globex/api search returned %d of acme's organization rules, want none: %+v", len(matches), matches)
	}

	// A repository that has no rule of its own, in the organization that does.
	query.OrgName, query.RepoName = testOrg, "inventory-service"
	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories for the owning organization returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].ID != stored.ID {
		t.Fatalf("acme/inventory-service search returned %+v, want the organization rule %s", matches, stored.ID)
	}
	if matches[0].Scope != ScopeOrg {
		t.Errorf("the match reports scope %s, want %s", matches[0].Scope, ScopeOrg)
	}
}

// T-040-02: when a repository's own rule and the organization's both match, the
// repository's is ranked higher, so a project exception is not overridden by a
// general default. The weight belongs to the caller, so no boost returns the
// ranking to the distance alone, and the number a caller reads back stays the
// distance either way.
func TestT040_02_RepoScopeOutranksOrgScope(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// The two rules sit at cosine 0.60 and 0.57 from the query, on different
	// axes so they are nothing like each other: close enough that only the
	// boost can decide between them, far enough apart that both are stored
	// rather than one being a duplicate of the other.
	org := testProposal("All services in the organization log with log/slog", embeddingLike(0.60, 1))
	org.Scope, org.PathGlob, org.Approved = ScopeOrg, "", true
	if _, _, err := db.ProposeMemory(ctx, org); err != nil {
		t.Fatalf("ProposeMemory(organization rule) returned error: %v", err)
	}

	repo := testProposal("legacy-worker keeps logrus and is exempt from the migration", embeddingLike(0.57, 2))
	repo.PathGlob, repo.Approved = "", true
	if _, _, err := db.ProposeMemory(ctx, repo); err != nil {
		t.Fatalf("ProposeMemory(repository rule) returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
		RepoScopeBoost: config.DefaultRepoScopeBoost,
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("search returned %d rules, want both candidates: %+v", len(matches), matches)
	}
	if matches[0].Scope != ScopeRepo {
		t.Errorf("the default boost of %v leads with the %s rule (similarities %v, %v), want the repository's own",
			config.DefaultRepoScopeBoost, matches[0].Scope, matches[0].Similarity, matches[1].Similarity)
	}
	// The boost decides the order and nothing else: the number the caller reads
	// back is the distance the embedding has, or a caller that ranks its own
	// results would be ranking on a score it cannot reproduce.
	if math.Abs(matches[0].Similarity-0.57) > 1e-5 {
		t.Errorf("the boosted rule reports similarity %v, want the distance 0.57 rather than the score", matches[0].Similarity)
	}

	query.RepoScopeBoost = 0
	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories without a boost returned error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("search without a boost returned %d rules, want both candidates: %+v", len(matches), matches)
	}
	if matches[0].Scope != ScopeOrg {
		t.Errorf("without a boost the ranking leads with the %s rule, want the closer one", matches[0].Scope)
	}
	if matches[0].Similarity <= matches[1].Similarity {
		t.Errorf("similarities are %v, want the first above the second", matches)
	}
}

// The boost ranks the candidates the query returns rather than a page that was
// already cut by the limit, so a repository rule that scores into the top five
// takes the place of the organization rule it overtakes (T-040-02).
func TestSearchMemoriesRanksBeforeTheLimit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Five organization rules at 0.90 down to 0.86, and a repository rule at
	// 0.845. Unboosted the repository rule is sixth, so a limit of five would
	// drop it; boosted it scores 0.895 and the 0.86 rule is the one that falls
	// outside instead.
	for i, cos := range []float64{0.90, 0.89, 0.88, 0.87, 0.86} {
		rule := testProposal("organization rule "+string(rune('a'+i)), embeddingLike(cos, i+1))
		rule.Scope, rule.PathGlob, rule.Approved = ScopeOrg, "", true
		if _, _, err := db.ProposeMemory(ctx, rule); err != nil {
			t.Fatalf("ProposeMemory(organization rule %d) returned error: %v", i, err)
		}
	}
	exception := testProposal("legacy-worker is exempt and keeps logrus", embeddingLike(0.845, 9))
	exception.PathGlob, exception.Approved = "", true
	if _, _, err := db.ProposeMemory(ctx, exception); err != nil {
		t.Fatalf("ProposeMemory(repository rule) returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: 98765432,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
		RepoScopeBoost: config.DefaultRepoScopeBoost,
		Limit:          5,
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 5 {
		t.Fatalf("search returned %d rules, want the limit of 5", len(matches))
	}
	if matches[1].Scope != ScopeRepo {
		t.Errorf("the repository's rule is in position %d (%s), want second: %+v", 1, matches[1].Scope, matches)
	}
	for _, match := range matches {
		if strings.Contains(match.RuleText, "organization rule e") {
			t.Errorf("the rule the repository's own overtook is still in the window: %+v", matches)
		}
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

	// The three rules sit at three different distances from the query, so the
	// ranking is decided by the embedding rather than by which id sorts first:
	// what this test is about is the glob, and a tie would test that instead.
	for _, rule := range []struct {
		text string
		glob string
		vec  []float32
	}{
		{"docs only", "docs/**", embeddingAt(0)},
		{"payments only", "internal/payments/**", embeddingTilted(0.9)},
		{"everywhere", "", embeddingTilted(0.5)},
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

// §10 item 5: a directive is ranked above every other candidate, so a rule a
// maintainer promoted above the repository's guidelines is never the rule a
// limit leaves out. The order is the only thing the authority changes: the
// similarity a caller reads back is still the distance.
func TestSearchMemoriesRanksADirectiveAboveACloserRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// The two rules sit at cosine 0.80 and 0.60 from the query, on different
	// axes so they are nothing like each other. The closer one is recorded
	// context, the further one a maintainer promoted.
	closer := testProposal("The validator tolerates 60s of clock skew", embeddingLike(0.80, 1))
	closer.PathGlob, closer.Approved = "", true
	if _, _, err := db.ProposeMemory(ctx, closer); err != nil {
		t.Fatalf("ProposeMemory(closer rule) returned error: %v", err)
	}

	further := testProposal("This repository pins its own clock source rather than the platform default", embeddingLike(0.60, 2))
	further.PathGlob, further.Approved = "", true
	stored, _, err := db.ProposeMemory(ctx, further)
	if err != nil {
		t.Fatalf("ProposeMemory(further rule) returned error: %v", err)
	}
	if _, err := db.ApproveMemory(ctx, further.InstallationID, stored.ID, MemoryDecision{
		Authority: AuthorityDirective, ReviewedBy: "octocat",
	}); err != nil {
		t.Fatalf("ApproveMemory returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: further.InstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
		RepoScopeBoost: config.DefaultRepoScopeBoost,
	}
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("search returned %d rules, want both candidates: %+v", len(matches), matches)
	}
	if matches[0].ID != stored.ID {
		t.Errorf("the ranking leads with the closer rule (similarities %v, %v), want the directive",
			matches[0].Similarity, matches[1].Similarity)
	}
	if matches[0].Authority != AuthorityDirective {
		t.Errorf("the leading rule carries authority %s, want %s", matches[0].Authority, AuthorityDirective)
	}
	// The promotion decides the order and nothing else: the number the caller
	// reads back is the distance the embedding has, or a caller that ranked its
	// own results would be ranking on a score it cannot reproduce.
	if math.Abs(matches[0].Similarity-0.60) > 1e-5 {
		t.Errorf("the directive reports similarity %v, want the distance 0.60 rather than a boosted score", matches[0].Similarity)
	}

	// What the promotion buys: a caller that asked for one rule is handed the
	// one a maintainer promoted.
	query.Limit = 1
	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories(limit 1) returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].ID != stored.ID {
		t.Errorf("a limit of one returned %+v, want the directive", matches)
	}
}

// §10 item 5: nothing on the proposal path raises a rule above the repository's
// guidelines, including the path that stores a rule approved without a moderator
// looking at it. A commenter cannot promote their own rule, and neither can an
// operator who turned the gate off.
func TestProposalIsAlwaysRecordedAsContext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for i, tc := range []struct {
		ruleText string
		approved bool
	}{
		{"a rule waiting for a decision", false},
		{"a rule the operator let through unchecked", true},
	} {
		proposal := testProposal(tc.ruleText, embeddingAt(i))
		proposal.Approved = tc.approved
		stored, inserted, err := db.ProposeMemory(ctx, proposal)
		if err != nil {
			t.Fatalf("ProposeMemory(approved=%v) returned error: %v", tc.approved, err)
		}
		if !inserted {
			t.Fatalf("ProposeMemory(approved=%v) stored nothing", tc.approved)
		}
		if stored.Authority != AuthorityContext {
			t.Errorf("approved=%v stored authority %s, want %s", tc.approved, stored.Authority, AuthorityContext)
		}
	}
}
