package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// Without a terminal on both sides there is nothing to move around with, so the
// browser prints the queue and names the subcommand that decides a rule.
func TestModWithoutATerminalPrintsTheQueue(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	waiting := seedModerationRule(t, dsn, "Clock skew up to 60 seconds is intentional")
	decided := seedModerationRule(t, dsn, "Settlements are retried with a bounded budget")
	if _, err := runModCmd(t, configPath, "approve", decided.ID[:8], "--moderator", "octocat"); err != nil {
		t.Fatalf("approve returned error: %v", err)
	}

	out, err := runModCmd(t, configPath)
	if err != nil {
		t.Fatalf("mod returned error: %v (output: %s)", err, out)
	}

	for _, want := range []string{waiting.ID[:8], decided.ID[:8], "PENDING", "APPROVED", "Clock skew", "rumil mod approve"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lost %q:\n%s", want, out)
		}
	}
}

// The queue is worked from the top: what is waiting comes before what was
// decided, and within a status the newest rule comes first, so the browser
// opens on the one that has been waiting for a decision.
func TestModTUIOrdersPendingFirst(t *testing.T) {
	dsn := modDatabase(t)
	llm := testutil.NewFakeLLM(t)
	configPath := modConfig(t, dsn, llm.URL())

	older := seedModerationRule(t, dsn, "The older waiting rule")
	newer := seedModerationRule(t, dsn, "The newer waiting rule")
	decided := seedModerationRule(t, dsn, "A rule that was already decided")
	if _, err := runModCmd(t, configPath, "approve", decided.ID[:8], "--moderator", "octocat"); err != nil {
		t.Fatalf("approve returned error: %v", err)
	}

	service, _, closeDB, err := openModeration(configPath)
	if err != nil {
		t.Fatalf("openModeration returned error: %v", err)
	}
	defer closeDB()

	tui := &modTUI{service: service, moderator: "octocat", in: strings.NewReader(""), out: io.Discard}
	memories, err := tui.rules(context.Background())
	if err != nil {
		t.Fatalf("rules returned error: %v", err)
	}
	if len(memories) != 3 {
		t.Fatalf("the browser holds %d rules, want 3", len(memories))
	}

	want := []string{newer.ID, older.ID, decided.ID}
	for i, memory := range memories {
		if memory.ID != want[i] {
			t.Errorf("position %d is %s, want %s", i, memory.ShortID(), want[i][:8])
		}
	}
}

// One line per rule has to carry what a maintainer picks by — which rule, what
// state it is in, where it applies — and stay one line.
func TestModTUIRuleLineCarriesTheChoice(t *testing.T) {
	memory := store.Memory{
		ID:        "5477cca2-ad54-42dd-92d1-d72b1dd191ba",
		OrgName:   "acme",
		RepoName:  "payments",
		Scope:     store.ScopeRepo,
		Authority: store.AuthorityContext,
		Status:    store.MemoryPending,
		RuleText:  strings.Repeat("every route that mutates state sits behind the admin middleware ", 4),
	}

	line := ruleLine(memory)
	for _, want := range []string{"5477cca2", "PENDING", "REPO", "acme/payments", "every route"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lost %q: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("the line is more than one line: %q", line)
	}
	if len(line) > 200 {
		t.Errorf("the line is %d characters, want a length a terminal can show", len(line))
	}
}

// The rule screen is what a maintainer decides on, so it carries everything the
// store recorded and nothing that has to be looked up elsewhere.
func TestModTUIRuleDetailCarriesTheRecord(t *testing.T) {
	created := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	memory := store.Memory{
		ID:            "5477cca2-ad54-42dd-92d1-d72b1dd191ba",
		OrgName:       "acme",
		RepoName:      "payments",
		Scope:         store.ScopeOrg,
		Authority:     store.AuthorityDirective,
		Status:        store.MemoryApproved,
		PathGlob:      "internal/httpapi/**",
		RuleText:      "Every route that mutates state sits behind the admin middleware.",
		Rationale:     "the author confirmed it in review",
		SourcePR:      42,
		SourceComment: "https://github.com/acme/payments/pull/42#discussion_r1",
		RequestedBy:   "octocat",
		ReviewedBy:    "alice",
		CreatedAt:     created,
	}

	detail := ruleDetail(memory)
	for _, want := range []string{
		"Every route that mutates state",
		"the author confirmed it in review",
		"acme (proposed in payments)",
		"DIRECTIVE",
		"internal/httpapi/**",
		"pull request #42",
		"octocat",
		"discussion_r1",
		"2026-03-01 12:00:00 UTC",
		"alice",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail lost %q:\n%s", want, detail)
		}
	}
}

// A rule with nothing recorded beside it still has to render, because an
// optional field the store left empty is not a reason to refuse the screen.
func TestModTUIRuleDetailWithoutOptionalFields(t *testing.T) {
	memory := store.Memory{
		ID:          "5477cca2-ad54-42dd-92d1-d72b1dd191ba",
		OrgName:     "acme",
		RepoName:    "payments",
		Scope:       store.ScopeRepo,
		Authority:   store.AuthorityContext,
		Status:      store.MemoryPending,
		RuleText:    "A rule recorded without a rationale.",
		RequestedBy: "octocat",
		CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
	}

	detail := ruleDetail(memory)
	for _, want := range []string{"A rule recorded without a rationale.", "Glob: -", "pull request #0"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail lost %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "Decided by") {
		t.Errorf("an undecided rule is reported as decided:\n%s", detail)
	}
}
