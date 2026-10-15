package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The moderation queue lists what is waiting, and only what is waiting: a
// maintainer works a filtered list rather than every rule the tenant ever
// recorded.
func TestListMemoriesFiltersByStatusAndRepository(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pending := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	if _, _, err := db.ProposeMemory(ctx, pending); err != nil {
		t.Fatalf("seeding a pending rule: %v", err)
	}
	approved := testProposal("Settlements are idempotent upserts", embeddingAt(1))
	approved.Approved = true
	if _, _, err := db.ProposeMemory(ctx, approved); err != nil {
		t.Fatalf("seeding an approved rule: %v", err)
	}
	otherRepo := testProposal("The worker pool drains on SIGTERM", embeddingAt(2))
	otherRepo.RepoName = "checkout"
	if _, _, err := db.ProposeMemory(ctx, otherRepo); err != nil {
		t.Fatalf("seeding a rule for another repository: %v", err)
	}

	t.Run("everything, newest first", func(t *testing.T) {
		memories, err := db.ListMemories(ctx, MemoryFilter{InstallationID: pending.InstallationID})
		if err != nil {
			t.Fatalf("ListMemories returned error: %v", err)
		}
		if len(memories) != 3 {
			t.Fatalf("listed %d memories, want 3", len(memories))
		}
		for i := 1; i < len(memories); i++ {
			if memories[i].CreatedAt.After(memories[i-1].CreatedAt) {
				t.Errorf("memories are not newest first: %+v", memories)
			}
		}
	})

	t.Run("only what is waiting", func(t *testing.T) {
		memories, err := db.ListMemories(ctx, MemoryFilter{InstallationID: pending.InstallationID, Status: MemoryPending})
		if err != nil {
			t.Fatalf("ListMemories returned error: %v", err)
		}
		if len(memories) != 2 {
			t.Fatalf("listed %d memories, want the two pending ones", len(memories))
		}
		for _, memory := range memories {
			if memory.Status != MemoryPending {
				t.Errorf("listed %s, want only %s", memory.Status, MemoryPending)
			}
		}
	})

	t.Run("one repository", func(t *testing.T) {
		memories, err := db.ListMemories(ctx, MemoryFilter{
			InstallationID: pending.InstallationID, OrgName: testOrg, RepoName: "checkout",
		})
		if err != nil {
			t.Fatalf("ListMemories returned error: %v", err)
		}
		if len(memories) != 1 || memories[0].RepoName != "checkout" {
			t.Errorf("listed %+v, want the rule recorded for checkout", memories)
		}
	})

	t.Run("another tenant sees nothing", func(t *testing.T) {
		memories, err := db.ListMemories(ctx, MemoryFilter{InstallationID: pending.InstallationID + 1})
		if err != nil {
			t.Fatalf("ListMemories returned error: %v", err)
		}
		if len(memories) != 0 {
			t.Errorf("another installation listed %+v, want nothing", memories)
		}
	})
}

// T-030-02: approving a rule records who decided and makes the rule
// retrievable.
func TestApproveMemoryMakesTheRuleRetrievable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional in the validator", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	approved, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{ReviewedBy: "octocat"})
	if err != nil {
		t.Fatalf("ApproveMemory returned error: %v", err)
	}
	if approved.Status != MemoryApproved {
		t.Errorf("status = %s, want %s", approved.Status, MemoryApproved)
	}
	if approved.ReviewedBy != "octocat" {
		t.Errorf("reviewed_by = %q, want the moderator", approved.ReviewedBy)
	}
	if approved.RuleText != stored.RuleText {
		t.Errorf("rule_text = %q, want the proposed text %q", approved.RuleText, stored.RuleText)
	}

	matches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].ID != stored.ID {
		t.Errorf("search returned %+v, want the approved rule", matches)
	}
}

// T-040-04 (store half): a decision that narrows an organization rule to the
// repository it was proposed in changes what the rule reaches, which is the
// whole point of the operation: another repository in the organization stops
// retrieving it, and the repository it was narrowed to still does.
func TestApproveMemoryNarrowsAnOrganizationRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Every service logs with log/slog and never with logrus", embeddingAt(0))
	proposal.Scope, proposal.PathGlob, proposal.Approved = ScopeOrg, "", true
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	query := MemoryQuery{
		InstallationID: proposal.InstallationID,
		OrgName:        testOrg,
		RepoName:       "inventory-service",
		Embedding:      embeddingAt(0),
	}
	// The proposal reaches every repository of the organization to begin with,
	// which is what makes the narrowing visible.
	matches, err := db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search before the decision returned %+v, want the organization rule", matches)
	}

	narrowed, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Scope: ScopeRepo, ReviewedBy: "octocat",
	})
	if err != nil {
		t.Fatalf("ApproveMemory returned error: %v", err)
	}
	if narrowed.Scope != ScopeRepo || narrowed.Status != MemoryApproved {
		t.Errorf("row is %+v, want an approved %s rule", narrowed, ScopeRepo)
	}
	if narrowed.RepoName != testRepo {
		t.Errorf("repo_name = %q, want the repository it was proposed from", narrowed.RepoName)
	}

	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("the narrowed rule still reaches inventory-service: %+v", matches)
	}

	query.RepoName = testRepo
	matches, err = db.SearchMemories(ctx, query)
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].ID != stored.ID {
		t.Errorf("search returned %+v, want the narrowed rule in the repository it was proposed in", matches)
	}
}

// T-030-03: an edited rule is approved against the vector for the edited text,
// not the one the proposal arrived with.
func TestApproveMemoryReEmbedsAnEditedRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	const edited = "Clock skew up to 60s is intentional in the JWT validator, which the IdP clocks differently"
	approved, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{RuleText: edited, Embedding: embeddingAt(7), ReviewedBy: "octocat"})
	if err != nil {
		t.Fatalf("ApproveMemory returned error: %v", err)
	}
	if approved.RuleText != edited {
		t.Errorf("rule_text = %q, want the edited text", approved.RuleText)
	}

	// The rule is found by the edited text's vector, and no longer by the one
	// the proposal carried.
	editedMatches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID, OrgName: testOrg, RepoName: testRepo, Embedding: embeddingAt(7),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(editedMatches) != 1 || editedMatches[0].ID != stored.ID {
		t.Errorf("the edited rule is not retrievable by its new text: %+v", editedMatches)
	}
	old, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID, OrgName: testOrg, RepoName: testRepo,
		Embedding: embeddingAt(0), FilePath: "pkg/auth/jwt.go", Limit: MemoryLimitMax,
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	for _, match := range old {
		if match.ID == stored.ID && match.Similarity > 0.5 {
			t.Errorf("the old vector still matches the rule (%.3f): the edit was not re-embedded", match.Similarity)
		}
	}
}

// T-030-04: a rejected rule stays on record and is inert.
func TestRejectMemoryKeepsTheRowAndHidesIt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	rejected, err := db.RejectMemory(ctx, proposal.InstallationID, stored.ID, "octocat")
	if err != nil {
		t.Fatalf("RejectMemory returned error: %v", err)
	}
	if rejected.Status != MemoryRejected {
		t.Errorf("status = %s, want %s", rejected.Status, MemoryRejected)
	}
	if rejected.ReviewedBy != "octocat" {
		t.Errorf("reviewed_by = %q, want the moderator", rejected.ReviewedBy)
	}
	if rejected.RuleText != stored.RuleText {
		t.Errorf("rejecting rewrote the rule text: %q", rejected.RuleText)
	}

	matches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID, OrgName: testOrg, RepoName: testRepo, Embedding: embeddingAt(0),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("a rejected rule is still retrievable: %+v", matches)
	}
	if got := countMemories(t, db); got != 1 {
		t.Errorf("memories = %d, want the rejected row kept", got)
	}
}

// A moderation command names one row, and a row of another tenant is not
// something it can reach or even detect.
func TestReviewMemoryNeedsTheRightTenantAndID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	if _, err := db.ApproveMemory(ctx, proposal.InstallationID+1, stored.ID, MemoryDecision{ReviewedBy: "octocat"}); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("another tenant's approval returned %v, want %v", err, ErrMemoryNotFound)
	}
	if _, err := db.RejectMemory(ctx, proposal.InstallationID, "00000000-0000-0000-0000-000000000000", "octocat"); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("an unknown id returned %v, want %v", err, ErrMemoryNotFound)
	}
	if _, err := db.MemoryByID(ctx, proposal.InstallationID, stored.ID); err != nil {
		t.Errorf("MemoryByID returned error: %v", err)
	}
	if _, err := db.MemoryByID(ctx, proposal.InstallationID+1, stored.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("another tenant's read returned %v, want %v", err, ErrMemoryNotFound)
	}
	if _, err := db.ListMemories(ctx, MemoryFilter{}); err == nil {
		t.Error("listing without an installation was allowed")
	}
}

// A decision is computed against a rule a moderator read, and the row can move
// under it: two moderators working the queue, or one page left open. Applying
// the stale decision would pair the words one of them read with the vector the
// other computed, so it is refused and the row is left as the winner left it.
func TestReviewMemoryRefusesADecisionAgainstStaleText(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	const edited = "Clock skew up to 60s is intentional in the JWT validator only"
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		RuleText: edited, Embedding: embeddingAt(7), ReviewedBy: "octocat",
	}); err != nil {
		t.Fatalf("the first approval returned error: %v", err)
	}

	// The second moderator read the rule before the first one landed: their
	// decision carries the text that is no longer there, and the vector for it
	// was never computed because the text looked unchanged.
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		RuleText: stored.RuleText, ReviewedBy: "hubot",
	}); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("a decision against the old text returned %v, want %v", err, ErrMemoryChanged)
	}

	after, err := db.MemoryByID(ctx, proposal.InstallationID, stored.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if after.RuleText != edited || after.ReviewedBy != "octocat" {
		t.Errorf("the refused decision changed the row: %+v", after)
	}
	matches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID, OrgName: testOrg, RepoName: testRepo, Embedding: embeddingAt(7),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].ID != stored.ID {
		t.Errorf("the rule is no longer found by the words it says: %+v", matches)
	}
}

// T-040-04 (store half): the statement itself refuses to record a repository
// rule for the whole organization. Two moderators who both read a rule as an
// organization rule, where the first narrows it, must not leave the second's
// widening behind: the rule would then reach every repository, which is what
// the narrowing was for. The second decision here was computed before the first
// landed, which is the shape that race takes when it is written down.
func TestReviewMemoryRefusesADecisionAgainstAStaleScope(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Every service logs with log/slog", embeddingAt(0))
	proposal.Scope, proposal.PathGlob, proposal.Approved = ScopeOrg, "", true
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	// Approving an organization rule as an organization rule is the ordinary
	// decision on such a row, and is not what the guard refuses.
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Scope: ScopeOrg, ReviewedBy: "hubot",
	}); err != nil {
		t.Fatalf("approving an organization rule as one returned error: %v", err)
	}

	// The first moderator reads the rule as an organization rule and narrows it.
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Scope: ScopeRepo, ReviewedBy: "octocat",
	}); err != nil {
		t.Fatalf("the narrowing returned error: %v", err)
	}

	// The second moderator's decision was computed before that write, so it
	// asks to record the rule for the organization again.
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Scope: ScopeOrg, ReviewedBy: "hubot",
	}); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("a decision against the old scope returned %v, want %v", err, ErrMemoryChanged)
	}

	after, err := db.MemoryByID(ctx, proposal.InstallationID, stored.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if after.Scope != ScopeRepo || after.ReviewedBy != "octocat" {
		t.Errorf("the refused decision changed the row: %+v", after)
	}
}

// Words without their vector cannot be written either: the row would then be
// found by text nobody approved.
func TestReviewMemoryRefusesTextWithoutItsEmbedding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Clock skew up to 60s is intentional", embeddingAt(0))
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		RuleText: "a rewritten rule", ReviewedBy: "octocat",
	}); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("approving with new text and no embedding returned %v, want %v", err, ErrMemoryChanged)
	}
	after, err := db.MemoryByID(ctx, proposal.InstallationID, stored.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if after.RuleText != stored.RuleText || after.Status != MemoryPending {
		t.Errorf("the refused decision changed the row: %+v", after)
	}
}

// A truncated id is what a confirmation reply puts on the pull request, and it
// is only ever ambiguous when two rules share it: the moderator is asked to be
// more specific rather than given whichever row sorted first.
func TestMemoryByIDReportsAnAmbiguousPrefix(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	const (
		installation = 98765432
		prefix       = "deadbeef-0000-4000-8000-"
	)
	ids := []string{prefix + "000000000001", prefix + "000000000002"}
	for i, id := range ids {
		if _, err := db.pool.Exec(ctx, `
			INSERT INTO memories (id, installation_id, org_name, repo_name, scope, status,
				rule_text, source_pr, source_comment, requested_by, embedding)
			VALUES ($1::uuid, $2, $3, $4, 'REPO', 'PENDING', $5, 7, 'comment', 'octocat', $6::vector)`,
			id, installation, testOrg, testRepo, fmt.Sprintf("rule %d", i), vectorLiteral(embeddingAt(i))); err != nil {
			t.Fatalf("inserting a rule with a chosen id: %v", err)
		}
	}

	if _, err := db.MemoryByID(ctx, installation, "deadbeef"); !errors.Is(err, ErrAmbiguousMemory) {
		t.Errorf("a shared prefix returned %v, want %v", err, ErrAmbiguousMemory)
	}
	if _, err := db.MemoryByID(ctx, installation+1, "deadbeef"); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("another tenant's prefix returned %v, want %v", err, ErrMemoryNotFound)
	}
	found, err := db.MemoryByID(ctx, installation, ids[0])
	if err != nil {
		t.Fatalf("a full id returned error: %v", err)
	}
	if found.ID != ids[0] {
		t.Errorf("read %s, want %s", found.ID, ids[0])
	}
}

// §10 item 5: the approval gate is where a rule is raised above the repository's
// own guidelines, and the row is what a later review reads.
func TestApproveMemoryPromotesARuleAboveTheGuidelines(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("This service keeps its own hand-rolled clock skew allowance", embeddingAt(0))
	proposal.Approved = true
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}
	if stored.Authority != AuthorityContext {
		t.Errorf("authority = %s, want %s for a rule no moderator has decided on", stored.Authority, AuthorityContext)
	}

	promoted, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Authority: AuthorityDirective, ReviewedBy: "octocat",
	})
	if err != nil {
		t.Fatalf("ApproveMemory returned error: %v", err)
	}
	if promoted.Authority != AuthorityDirective {
		t.Errorf("authority = %s, want %s", promoted.Authority, AuthorityDirective)
	}

	// The promotion survives the round trip a review makes, which is what the
	// reader of a candidate rule has to be able to see.
	matches, err := db.SearchMemories(ctx, MemoryQuery{
		InstallationID: proposal.InstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      embeddingAt(0),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].Authority != AuthorityDirective {
		t.Fatalf("search returned %+v, want one rule carrying %s", matches, AuthorityDirective)
	}

	// A moderator can take it back, so the promotion settles a rule's standing
	// rather than opening a one-way door.
	demoted, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Authority: AuthorityContext, ReviewedBy: "octocat",
	})
	if err != nil {
		t.Fatalf("ApproveMemory(demote) returned error: %v", err)
	}
	if demoted.Authority != AuthorityContext {
		t.Errorf("authority = %s, want it back at %s", demoted.Authority, AuthorityContext)
	}

	// A decision that names no authority leaves the rule's own standing alone, so
	// a moderator working the queue after somebody promoted a rule does not have
	// to know about the promotion to avoid undoing it.
	if _, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Authority: AuthorityDirective, ReviewedBy: "octocat",
	}); err != nil {
		t.Fatalf("ApproveMemory(promote again) returned error: %v", err)
	}
	kept, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		RuleText:  "This service keeps its own hand-rolled clock skew allowance, and clocks its own way",
		Embedding: embeddingAt(0), ReviewedBy: "hubot",
	})
	if err != nil {
		t.Fatalf("ApproveMemory(edit only) returned error: %v", err)
	}
	if kept.Authority != AuthorityDirective || kept.ReviewedBy != "hubot" {
		t.Errorf("row is %+v, want the promotion kept while the edit lands", kept)
	}
}

// §10 item 5: a promotion applies to one repository. The statement refuses the
// pair rather than trusting a surface to have computed it, so an organization
// rule cannot be made to outrank every repository's own guidelines in the
// organization, and the row is left alone when it is tried.
func TestReviewMemoryRefusesAPromotionThatReachesTheOrganization(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	proposal := testProposal("Every service in the organization logs with log/slog", embeddingAt(0))
	proposal.Scope, proposal.PathGlob, proposal.Approved = ScopeOrg, "", true
	stored, _, err := db.ProposeMemory(ctx, proposal)
	if err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	}

	_, err = db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Authority: AuthorityDirective, ReviewedBy: "octocat",
	})
	if !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("promoting an organization-wide rule returned %v, want %v", err, ErrMemoryChanged)
	}

	after, err := db.MemoryByID(ctx, proposal.InstallationID, stored.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if after.Scope != ScopeOrg || after.Authority != AuthorityContext {
		t.Errorf("row is %+v, want it unchanged at %s/%s", after, ScopeOrg, AuthorityContext)
	}

	// Narrowing the rule in the same decision is what makes the promotion land,
	// which is the path both moderation surfaces offer.
	narrowed, err := db.ApproveMemory(ctx, proposal.InstallationID, stored.ID, MemoryDecision{
		Scope: ScopeRepo, Authority: AuthorityDirective, ReviewedBy: "octocat",
	})
	if err != nil {
		t.Fatalf("ApproveMemory(narrow and promote) returned error: %v", err)
	}
	if narrowed.Scope != ScopeRepo || narrowed.Authority != AuthorityDirective {
		t.Errorf("row is %+v, want a %s rule carrying %s", narrowed, ScopeRepo, AuthorityDirective)
	}
}
