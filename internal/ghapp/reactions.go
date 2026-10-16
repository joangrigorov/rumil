package ghapp

import (
	"context"
	"fmt"
)

// ReactionAcknowledged is the reaction Rumil answers a request with. A person
// who asks for a review has no other sign that the request was picked up: the
// review itself arrives a minute later, and a request that was dropped looks
// the same as one that is running.
const ReactionAcknowledged = "+1"

// ReactToPullRequest acknowledges on the pull request itself. A review request
// carries no comment to answer, so the pull request is the only surface the
// person who asked is looking at.
func (c *Client) ReactToPullRequest(ctx context.Context, content string) error {
	if _, _, err := c.gh.Reactions.CreateIssueReaction(ctx, c.owner, c.repo, c.number, content); err != nil {
		return fmt.Errorf("reacting to %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}
	return nil
}

// ReactToComment acknowledges on the comment that carried a command. A review
// comment is reacted to through its own endpoint and a conversation comment
// through the issues one, because they are different objects with different
// routes.
//
// Reacting twice is not an error: GitHub answers with the reaction that is
// already there, so a retried command does not accumulate them.
func (c *Client) ReactToComment(ctx context.Context, commentID int64, reviewComment bool, content string) error {
	var err error
	if reviewComment {
		_, _, err = c.gh.Reactions.CreatePullRequestCommentReaction(ctx, c.owner, c.repo, commentID, content)
	} else {
		_, _, err = c.gh.Reactions.CreateIssueCommentReaction(ctx, c.owner, c.repo, commentID, content)
	}
	if err != nil {
		return fmt.Errorf("reacting to comment %d on %s/%s#%d: %w", commentID, c.owner, c.repo, c.number, err)
	}
	return nil
}
