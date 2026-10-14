package store

import (
	"context"
	"errors"
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
