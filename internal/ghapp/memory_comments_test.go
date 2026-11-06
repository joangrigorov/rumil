package ghapp

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func newTestMemoryCommentEditor(t *testing.T, gh *testutil.FakeGitHub) *MemoryCommentEditor {
	t.Helper()

	auth, err := NewAppAuth(config.GitHub{AppID: 123, InstallationID: 98765432}, testutil.TestPrivateKeyPEM(t), gh.URL())
	if err != nil {
		t.Fatalf("NewAppAuth returned error: %v", err)
	}
	return NewMemoryCommentEditor(auth)
}

func TestRewriteMemoryCommentsUpdatesEveryConfirmation(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(10, 0, "rumil[bot]", "Memory proposal `#abc12345` queued for maintainer moderation (`status: PENDING`)."),
		comment(11, 10, "rumil[bot]", "Memory `#abc12345` already covers this (`status: PENDING`)."),
		comment(12, 0, "octocat", "Memory proposal `#abc12345` queued for maintainer moderation (`status: PENDING`)."),
		comment(13, 0, "rumil[bot]", "Memory proposal `#deadbee1` queued for maintainer moderation (`status: PENDING`)."),
		comment(14, 0, "rumil[bot]", "A finding that mentions #abc12345 without a status."),
	}
	gh.IssueComments = []testutil.CommentEntry{
		comment(20, 0, "rumil", "Memory proposal `#ABC12345` queued for maintainer moderation (`status: PENDING`)."),
		comment(21, 0, "octocat", "Memory proposal `#abc12345` queued for maintainer moderation (`status: PENDING`)."),
	}

	outcome := "Memory `#abc12345` approved (`status: APPROVED`, scope: REPO). It is available to reviews."
	edited, err := newTestMemoryCommentEditor(t, gh).RewriteMemoryComments(context.Background(), 98765432, gh.Owner, gh.Repo, gh.Number, "abc12345", outcome)
	if err != nil {
		t.Fatalf("RewriteMemoryComments returned error: %v", err)
	}
	if edited != 3 {
		t.Fatalf("edited %d comments, want 3", edited)
	}

	edits := gh.EditedComments()
	if len(edits) != 3 {
		t.Fatalf("the fake recorded %d edits, want 3: %+v", len(edits), edits)
	}
	wantTargets := []testutil.EditedComment{
		{Target: "review_comment", ID: 10, Body: outcome},
		{Target: "review_comment", ID: 11, Body: outcome},
		{Target: "issue_comment", ID: 20, Body: outcome},
	}
	for i, want := range wantTargets {
		if edits[i] != want {
			t.Errorf("edit %d = %+v, want %+v", i, edits[i], want)
		}
	}

	for _, tc := range []struct {
		id       int64
		comments []testutil.CommentEntry
		want     string
	}{
		{10, gh.Comments, outcome},
		{11, gh.Comments, outcome},
		{20, gh.IssueComments, outcome},
	} {
		body := ""
		for _, comment := range tc.comments {
			if comment.ID == tc.id {
				body = comment.Body
			}
		}
		if body != tc.want {
			t.Errorf("comment %d body = %q, want %q", tc.id, body, tc.want)
		}
	}

	for _, tc := range []struct {
		id       int64
		comments []testutil.CommentEntry
	}{
		{12, gh.Comments},
		{13, gh.Comments},
		{14, gh.Comments},
		{21, gh.IssueComments},
	} {
		for _, comment := range tc.comments {
			if comment.ID == tc.id && strings.Contains(comment.Body, "approved") {
				t.Errorf("comment %d was rewritten: %q", tc.id, comment.Body)
			}
		}
	}
}

func TestRewriteMemoryCommentsLeavesCommentsWithoutAMatchAlone(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(10, 0, "rumil[bot]", "Memory proposal `#deadbee1` queued (`status: PENDING`)."),
	}
	gh.IssueComments = []testutil.CommentEntry{
		comment(20, 0, "rumil", "Memory proposal `#deadbee1` queued (`status: PENDING`)."),
	}

	edited, err := newTestMemoryCommentEditor(t, gh).RewriteMemoryComments(context.Background(), 98765432, gh.Owner, gh.Repo, gh.Number, "abc12345", "Memory `#abc12345` approved.")
	if err != nil {
		t.Fatalf("RewriteMemoryComments returned error: %v", err)
	}
	if edited != 0 {
		t.Errorf("edited %d comments, want 0", edited)
	}
	if len(gh.EditedComments()) != 0 {
		t.Errorf("the fake recorded edits: %+v", gh.EditedComments())
	}
}

func TestRewriteMemoryCommentsReportsAnEditFailure(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(10, 0, "rumil[bot]", "Memory proposal `#abc12345` queued (`status: PENDING`)."),
	}
	gh.IssueComments = []testutil.CommentEntry{
		comment(20, 0, "rumil", "Memory proposal `#abc12345` queued (`status: PENDING`)."),
	}
	gh.EditStatus = 404

	edited, err := newTestMemoryCommentEditor(t, gh).RewriteMemoryComments(context.Background(), 98765432, gh.Owner, gh.Repo, gh.Number, "abc12345", "Memory `#abc12345` approved.")
	if err == nil {
		t.Fatal("RewriteMemoryComments hid the edit failure")
	}
	if edited != 0 {
		t.Errorf("edited %d comments, want 0", edited)
	}
}
