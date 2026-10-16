package ghapp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// A review request carries no comment, so the acknowledgement goes on the pull
// request itself: that is the surface the person who asked is looking at.
func TestReactToPullRequestAcknowledgesTheRequest(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	if err := client.ReactToPullRequest(context.Background(), ReactionAcknowledged); err != nil {
		t.Fatalf("ReactToPullRequest returned error: %v", err)
	}

	reactions := gh.AddedReactions()
	if len(reactions) != 1 {
		t.Fatalf("recorded %d reactions, want 1", len(reactions))
	}
	if reactions[0].Target != "pull_request" {
		t.Errorf("target = %q, want the pull request body", reactions[0].Target)
	}
	if reactions[0].Content != "+1" {
		t.Errorf("content = %q, want %q", reactions[0].Content, ReactionAcknowledged)
	}
}

// A review comment and a conversation comment are different objects on
// different routes, so the acknowledgement has to take the right one.
func TestReactToCommentPicksTheRightEndpoint(t *testing.T) {
	cases := []struct {
		name          string
		reviewComment bool
		wantTarget    string
	}{
		{name: "a reply in a review thread", reviewComment: true, wantTarget: "review_comment"},
		{name: "a comment in the conversation", reviewComment: false, wantTarget: "issue_comment"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			client := newTestClient(t, gh)

			if err := client.ReactToComment(context.Background(), 4711, tc.reviewComment, ReactionAcknowledged); err != nil {
				t.Fatalf("ReactToComment returned error: %v", err)
			}

			reactions := gh.AddedReactions()
			if len(reactions) != 1 {
				t.Fatalf("recorded %d reactions, want 1", len(reactions))
			}
			if reactions[0].Target != tc.wantTarget {
				t.Errorf("target = %q, want %q", reactions[0].Target, tc.wantTarget)
			}
			if reactions[0].ID != 4711 {
				t.Errorf("id = %d, want the comment that carried the command", reactions[0].ID)
			}
		})
	}
}

func TestReactToCommentReportsARefusal(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ReactionStatus = http.StatusForbidden
	client := newTestClient(t, gh)

	err := client.ReactToComment(context.Background(), 4711, true, ReactionAcknowledged)
	if err == nil {
		t.Fatal("ReactToComment accepted a refused reaction")
	}
	if !strings.Contains(err.Error(), "4711") {
		t.Errorf("error = %q, want it to name the comment", err)
	}
}
