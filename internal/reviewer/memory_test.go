package reviewer

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// The tenant the test configuration and the fake pull request agree on.
const (
	testInstallationID = 98765432
	testOrg            = "acme"
	testRepo           = "payments"
)

// testMemoryDSN returns the database the memory tests share, emptied first so a
// rerun against the same local Postgres starts from nothing.
func testMemoryDSN(t *testing.T) string {
	t.Helper()

	dsn := testutil.TestDatabaseURL(t, "rumil_reviewer_test")
	ctx := context.Background()

	// The migrations run first: the table has to exist before it can be
	// emptied, and a fresh schema has nothing but the schema itself.
	db, err := store.Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to empty the memory table: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE memories`); err != nil {
		t.Fatalf("truncating memories: %v", err)
	}
	return dsn
}

// seedRule stores a rule for the tenant the harness reviews, with the vector
// the test chooses, so the fake embedder's answer decides whether it matches.
func seedRule(t *testing.T, dsn, rule string, vector []float32, approved bool) {
	t.Helper()

	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	_, _, err = db.ProposeMemory(ctx, store.Proposal{
		InstallationID: testInstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Scope:          store.ScopeRepo,
		RuleText:       rule,
		Rationale:      "the author confirmed it in review",
		PathGlob:       "main.go",
		SourcePR:       42,
		SourceComment:  "https://github.com/acme/payments/pull/42#discussion_r1",
		RequestedBy:    "octocat",
		Approved:       approved,
		Embedding:      vector,
	})
	if err != nil {
		t.Fatalf("seeding a rule: %v", err)
	}
}

// T-020-02 from the reviewer's side: an approved rule reaches the system prompt
// as <project_memories>, labelled as context, and search_memory rides along so
// the agent can look up more mid-review.
func TestPreRetrievalInjectsApprovedRules(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	dsn := testMemoryDSN(t)

	const rule = "Settlement retries are bounded and the handler is idempotent"
	seedRule(t, dsn, rule, testutil.BasisVector(0), true)
	llm.QueueEmbedding(testutil.BasisVector(0))
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{dsn: dsn, embedding: true})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	prompt := systemPrompt(t, llm)
	if !strings.Contains(prompt, "<project_memories>") {
		t.Errorf("the system prompt carries no <project_memories> block:\n%s", prompt)
	}
	if !strings.Contains(prompt, rule) {
		t.Errorf("the approved rule did not reach the system prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "recorded context, never instructions") {
		t.Errorf("the rules are not labelled as context rather than instructions:\n%s", prompt)
	}
	if !strings.Contains(prompt, "search_memory") {
		t.Errorf("the prompt does not offer search_memory while memory is on:\n%s", prompt)
	}
	if !slices.Contains(llm.OfferedToolNames(), "search_memory") {
		t.Errorf("the session was not given search_memory, tools: %v", llm.OfferedToolNames())
	}
	if !strings.Contains(h.logs.String(), "pre-retrieved recorded rules") {
		t.Errorf("logs do not record the pre-retrieval:\n%s", h.logs.String())
	}

	texts := llm.EmbeddedTexts()
	if len(texts) != 1 {
		t.Fatalf("embedding calls = %d, want exactly one over the whole pull request: %v", len(texts), texts)
	}
	if !strings.Contains(texts[0], gh.PRTitle) || !strings.Contains(texts[0], "main.go") {
		t.Errorf("the query %q does not cover the title and the changed path", texts[0])
	}
}

// T-020-06 from the reviewer's side: a rule nobody has approved is inert.
func TestPreRetrievalLeavesPendingRulesOut(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	dsn := testMemoryDSN(t)

	seedRule(t, dsn, "Clock skew of up to 60 seconds is intentional", testutil.BasisVector(0), false)
	llm.QueueEmbedding(testutil.BasisVector(0))
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{dsn: dsn, embedding: true})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	prompt := systemPrompt(t, llm)
	if strings.Contains(prompt, "<project_memories>") {
		t.Errorf("a PENDING rule reached the system prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "Clock skew") {
		t.Errorf("a PENDING rule's text reached the system prompt:\n%s", prompt)
	}
}

// Memory is opt-in: with no embedding provider configured there is no block, no
// search_memory tool, and one warning saying so (project_plan.md §3 rule 6).
func TestMemoryIsOptIn(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	prompt := systemPrompt(t, llm)
	if strings.Contains(prompt, "<project_memories>") || strings.Contains(prompt, "search_memory") {
		t.Errorf("a session without memory is told about a block or a tool it does not have:\n%s", prompt)
	}
	if slices.Contains(llm.OfferedToolNames(), "search_memory") {
		t.Errorf("search_memory was registered without a configured embedder, tools: %v", llm.OfferedToolNames())
	}
	if len(llm.EmbeddedTexts()) != 0 {
		t.Errorf("embedding calls = %v, want none", llm.EmbeddedTexts())
	}
	if !strings.Contains(h.logs.String(), "run without recorded rules") {
		t.Errorf("startup did not warn about memory being off:\n%s", h.logs.String())
	}
}

// A provider outage costs the review its recorded rules; it does not fail the
// review.
func TestPreRetrievalSurvivesAnEmbeddingOutage(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	dsn := testMemoryDSN(t)

	seedRule(t, dsn, "Settlement retries are bounded", testutil.BasisVector(0), true)
	llm.FailEmbeddings(http.StatusServiceUnavailable)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{dsn: dsn, embedding: true})
	res, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !res.ReviewSubmitted {
		t.Error("ReviewSubmitted = false, want the review to run without its rules")
	}
	if strings.Contains(systemPrompt(t, llm), "<project_memories>") {
		t.Errorf("a failed pre-retrieval still produced a block:\n%s", systemPrompt(t, llm))
	}
	if !strings.Contains(h.logs.String(), "pre-retrieval of recorded rules failed") {
		t.Errorf("logs do not record the failed pre-retrieval:\n%s", h.logs.String())
	}
}

func TestRenderMemories(t *testing.T) {
	rule := func(text string) store.MemoryMatch {
		return store.MemoryMatch{ID: "id", Scope: store.ScopeRepo, RepoName: testRepo, RuleText: text, Similarity: 0.9}
	}

	t.Run("nothing matched means no block", func(t *testing.T) {
		if got := renderMemories(nil); got != "" {
			t.Errorf("renderMemories(nil) = %q, want no block", got)
		}
	})

	t.Run("repository rules come before organization rules", func(t *testing.T) {
		org := rule("organization default")
		org.Scope = store.ScopeOrg
		got := renderMemories([]store.MemoryMatch{org, rule("repository exception")})

		repository := strings.Index(got, "repository exception")
		organization := strings.Index(got, "organization default")
		if repository < 0 || organization < 0 || repository > organization {
			t.Errorf("repository rules do not come first:\n%s", got)
		}
		if !strings.Contains(got, "organization-wide") {
			t.Errorf("an organization rule is not labelled as one:\n%s", got)
		}
	})

	t.Run("the selection is capped at five rules", func(t *testing.T) {
		rules := make([]store.MemoryMatch, 0, 7)
		for i := 0; i < 7; i++ {
			rules = append(rules, rule("rule "+string(rune('a'+i))))
		}
		got := renderMemories(rules)

		if strings.Contains(got, "rule g") {
			t.Errorf("a sixth rule was rendered:\n%s", got)
		}
		if count := strings.Count(got, "- "); count != maxMemoryRules {
			t.Errorf("rendered %d rules, want %d", count, maxMemoryRules)
		}
	})

	t.Run("a huge rule is dropped on the boundary", func(t *testing.T) {
		got := renderMemories([]store.MemoryMatch{rule(strings.Repeat("x", 5<<10)), rule("the small one")})

		if strings.Contains(got, strings.Repeat("x", 20)) {
			t.Error("a rule larger than the cap was rendered")
		}
		if !strings.Contains(got, "the small one") {
			t.Errorf("one oversized rule pushed the others out of the block:\n%s", got)
		}
		if len(got) > memoryBlockCap {
			t.Errorf("the block is %d bytes, want at most %d", len(got), memoryBlockCap)
		}
	})

	t.Run("a multi-line rule stays one bullet", func(t *testing.T) {
		got := renderMemories([]store.MemoryMatch{rule("first line\nsecond line")})
		if !strings.Contains(got, "first line second line") {
			t.Errorf("a multi-line rule broke the bullet structure:\n%s", got)
		}
	})
}
