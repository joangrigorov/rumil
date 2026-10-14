package ghapp

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

func TestLastReviewedCommitReportsTheNewestReviewByTheBot(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ExistingReviews = []testutil.ReviewEntry{
		{ID: 1, Author: "rumil[bot]", State: "COMMENTED", Body: "round one", CommitID: "aaaa1111"},
		{ID: 2, Author: "octocat", State: "COMMENTED", Body: "a person's review", CommitID: "cccc3333"},
		{ID: 3, Author: "rumil[bot]", State: "COMMENTED", Body: "round two", CommitID: "bbbb2222"},
	}
	client := newTestClient(t, gh)

	commit, err := client.LastReviewedCommit(context.Background(), "@rumil")
	if err != nil {
		t.Fatalf("LastReviewedCommit returned error: %v", err)
	}
	if commit != "bbbb2222" {
		t.Errorf("commit = %q, want the bot's newest review", commit)
	}
}

// A pull request Rumil has never reviewed has no delta to measure from, and a
// reviewer still needs the full diff rather than an empty one.
func TestLastReviewedCommitIsEmptyWithoutABotReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ExistingReviews = []testutil.ReviewEntry{
		{ID: 1, Author: "octocat", State: "COMMENTED", Body: "a person's review", CommitID: "cccc3333"},
	}
	client := newTestClient(t, gh)

	commit, err := client.LastReviewedCommit(context.Background(), "@rumil")
	if err != nil {
		t.Fatalf("LastReviewedCommit returned error: %v", err)
	}
	if commit != "" {
		t.Errorf("commit = %q, want none", commit)
	}
}

// T-020-09: the diff of a later round covers only what was pushed since the
// reviewed commit.
func TestDiffSinceComparesOnlyTheNewCommits(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.SinceDiff = strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 2222222..3333333 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -10,4 +10,6 @@ func main() {",
		" \tif err != nil {",
		" \t\tlog.Fatal(err)",
		" \t}",
		"+	// and a lock around it",
		"+	settleLocked(ctx)",
		" \tdefer conn.Close()",
		"",
	}, "\n")
	client := newTestClient(t, gh)

	diff, err := client.DiffSince(context.Background(), gh.HeadSHA, "3333333333333333333333333333333333333333")
	if err != nil {
		t.Fatalf("DiffSince returned error: %v", err)
	}
	if !diff.HasNewLine("main.go", 13) {
		t.Errorf("the compared diff lost the new line: %+v", diff.Paths())
	}
	if !strings.Contains(strings.Join(gh.RequestLog(), "\n"), "/compare/") {
		t.Errorf("the partial diff did not go through the compare endpoint:\n%s", strings.Join(gh.RequestLog(), "\n"))
	}
}

func TestDiffSinceRequiresBothCommits(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	if _, err := client.DiffSince(context.Background(), "", gh.HeadSHA); err == nil {
		t.Error("DiffSince accepted an empty base commit")
	}
}

func TestConversationCommentsReadTheConversation(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.IssueComments = []testutil.CommentEntry{
		testutil.ReviewComment(1, 0, "octocat", "first thought"),
		testutil.ReviewComment(2, 0, "rumil[bot]", "Reviewed against the repository guidelines."),
	}
	client := newTestClient(t, gh)

	comments, err := client.ConversationComments(context.Background())
	if err != nil {
		t.Fatalf("ConversationComments returned error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("read %d comments, want 2", len(comments))
	}
	if comments[0].Body != "first thought" || comments[0].Author != "octocat" {
		t.Errorf("first comment = %+v, want the oldest one first", comments[0])
	}
	if comments[1].CreatedAt.IsZero() {
		t.Error("the comment carries no timestamp")
	}
}
