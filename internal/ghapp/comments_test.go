package ghapp

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// comment builds a fixture entry the fake serves on the comments endpoints.
func comment(id, inReplyTo int64, author, body string) testutil.CommentEntry {
	return testutil.ReviewComment(id, inReplyTo, author, body)
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
// attempt already posted, on both kinds of command.
func TestCommentBodiesReadTheCommandThread(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		comment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
		comment(2, 1, "octocat", "@rumil remember we do that on purpose"),
		comment(3, 2, "rumil[bot]", "Memory proposal `#9f1c2a4b` queued for maintainer moderation."),
		comment(4, 0, "rumil[bot]", "An unrelated thread."),
		comment(5, 4, "rumil[bot]", "Memory `#deadbee1` already covers this (`status: APPROVED`)."),
	}
	gh.IssueComments = []testutil.CommentEntry{
		comment(6, 0, "reviewer", "@rumil remember to close rows we migrate"),
		comment(7, 0, "rumil", "Memory `#0ddba11` recorded (`status: APPROVED`)."),
	}
	client := newTestClient(t, gh)

	t.Run("a command's own thread is what is read", func(t *testing.T) {
		bodies, err := client.ThreadCommentBodies(context.Background(), 2, "@rumil")
		if err != nil {
			t.Fatalf("ThreadCommentBodies returned error: %v", err)
		}
		joined := strings.Join(bodies, "\n")
		if !strings.Contains(joined, "#9f1c2a4b") {
			t.Errorf("the answer in the command's thread was not read: %v", bodies)
		}
		if strings.Contains(joined, "#deadbee1") {
			t.Errorf("another thread's answer was attributed to this command: %v", bodies)
		}
		if strings.Contains(joined, "we do that on purpose") {
			t.Errorf("another author's comment was attributed to Rumil: %v", bodies)
		}
	})

	t.Run("conversation answers are read separately", func(t *testing.T) {
		bodies, err := client.ConversationCommentBodies(context.Background(), "@rumil")
		if err != nil {
			t.Fatalf("ConversationCommentBodies returned error: %v", err)
		}
		joined := strings.Join(bodies, "\n")
		if !strings.Contains(joined, "#0ddba11") {
			t.Errorf("the conversation answer was not read: %v", bodies)
		}
		if strings.Contains(joined, "#9f1c2a4b") {
			t.Errorf("a review-thread answer was read as a conversation one: %v", bodies)
		}
		if strings.Contains(joined, "to close rows we migrate") {
			t.Errorf("another author's comment was attributed to Rumil: %v", bodies)
		}
	})
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
		// The command sits on comment 3 and replies to comment 2, which is the
		// chain the walk starts from.
		body, err := client.ThreadCommentBy(context.Background(), 2, "@rumil")
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

		body, err := client.ThreadCommentBy(context.Background(), 1, "@rumil")
		if err != nil {
			t.Fatalf("ThreadCommentBy returned error: %v", err)
		}
		if body != "" {
			t.Errorf("walked to %q, want nothing", body)
		}
	})
}
