package ghapp

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// comment builds a fixture entry the fake serves on the comments endpoints.
func comment(id, inReplyTo int64, author, body string) testutil.CommentEntry {
	entry := testutil.CommentEntry{ID: id, Path: "main.go", Line: 12, Body: body, InReplyToID: inReplyTo}
	entry.User.Login = author
	entry.User.Type = "User"
	return entry
}

func TestNormalizeLogin(t *testing.T) {
	for _, tc := range []struct{ handle, want string }{
		{"@rumil", "rumil"},
		{"rumil", "rumil"},
		{"rumil[bot]", "rumil"},
		{"  @Rumil[bot]  ", "rumil"},
		{"", ""},
	} {
		if got := NormalizeLogin(tc.handle); got != tc.want {
			t.Errorf("NormalizeLogin(%q) = %q, want %q", tc.handle, got, tc.want)
		}
	}
}

// T-020-01 (client half): the confirmation reply lands in the thread the command
// arrived on.
func TestReplyToReviewCommentAnswersInTheThread(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	if err := client.ReplyToReviewComment(context.Background(), 4711, "Memory proposal `#9f1c2a4b` queued."); err != nil {
		t.Fatalf("ReplyToReviewComment returned error: %v", err)
	}

	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want 1", len(posted))
	}
	if posted[0].InReplyTo != 4711 {
		t.Errorf("reply carries in_reply_to %d, want 4711", posted[0].InReplyTo)
	}
	if !strings.Contains(posted[0].Body, "#9f1c2a4b") {
		t.Errorf("reply body = %q, want it to name the memory", posted[0].Body)
	}
}

func TestCreateIssueCommentPostsAConversationComment(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	if err := client.CreateIssueComment(context.Background(), "Memory proposal `#9f1c2a4b` queued."); err != nil {
		t.Fatalf("CreateIssueComment returned error: %v", err)
	}

	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want 1", len(posted))
	}
	if posted[0].InReplyTo != 0 {
		t.Errorf("a conversation comment carries in_reply_to %d, want none", posted[0].InReplyTo)
	}
}

// The retry that returns a job to the queue has to see the answer the previous
// attempt already posted, whichever endpoint carried it.
func TestCommentsByAuthorReadsBothEndpoints(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(1, 0, "rumil[bot]", "Memory proposal `#9f1c2a4b` queued for maintainer moderation."),
		comment(2, 0, "octocat", "please remember this"),
	}
	gh.IssueComments = []testutil.CommentEntry{
		comment(3, 0, "rumil", "Reviewed against the repository guidelines."),
		comment(4, 0, "reviewer", "looks good"),
	}
	client := newTestClient(t, gh)

	bodies, err := client.CommentsByAuthor(context.Background(), "@rumil")
	if err != nil {
		t.Fatalf("CommentsByAuthor returned error: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("read %d comments, want Rumil's two: %v", len(bodies), bodies)
	}
	if !strings.Contains(strings.Join(bodies, "\n"), "#9f1c2a4b") {
		t.Errorf("the comments Rumil posted do not include the answer: %v", bodies)
	}
	for _, body := range bodies {
		if strings.Contains(body, "please remember this") || strings.Contains(body, "looks good") {
			t.Errorf("another author's comment was attributed to Rumil: %q", body)
		}
	}
}

func TestThreadCommentByWalksUpTheThread(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
		comment(2, 1, "octocat", "we do that on purpose"),
		comment(3, 2, "reviewer", "still, a timeout would help"),
		comment(4, 0, "rumil[bot]", "An unrelated thread."),
	}
	client := newTestClient(t, gh)

	t.Run("a reply two levels down still finds Rumil's root", func(t *testing.T) {
		body, err := client.ThreadCommentBy(context.Background(), 3, "@rumil")
		if err != nil {
			t.Fatalf("ThreadCommentBy returned error: %v", err)
		}
		if !strings.Contains(body, "without a timeout") {
			t.Errorf("walked to %q, want Rumil's root comment", body)
		}
	})

	t.Run("a thread Rumil never opened has nothing to find", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.Comments = []testutil.CommentEntry{
			comment(1, 0, "octocat", "why is this here?"),
			comment(2, 1, "reviewer", "good question"),
		}
		client := newTestClient(t, gh)

		body, err := client.ThreadCommentBy(context.Background(), 2, "@rumil")
		if err != nil {
			t.Fatalf("ThreadCommentBy returned error: %v", err)
		}
		if body != "" {
			t.Errorf("walked to %q, want nothing", body)
		}
	})
}
