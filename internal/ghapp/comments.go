package ghapp

import (
	"context"
	"fmt"

	"github.com/google/go-github/v69/github"
)

// threadDepthLimit bounds a reply walk. Threads are shallow by construction, so
// the limit only exists so a malformed payload cannot spin the loop.
const threadDepthLimit = 50

// ReplyToReviewComment answers a review comment inside its thread, which is
// where the person who asked Rumil to remember something is looking.
func (c *Client) ReplyToReviewComment(ctx context.Context, commentID int64, body string) error {
	if _, _, err := c.gh.PullRequests.CreateCommentInReplyTo(ctx, c.owner, c.repo, c.number, body, commentID); err != nil {
		return fmt.Errorf("replying to comment %d on %s/%s#%d: %w", commentID, c.owner, c.repo, c.number, err)
	}
	return nil
}

// CreateIssueComment posts a conversation comment on the pull request, which is
// where the answer to a top-level command belongs.
func (c *Client) CreateIssueComment(ctx context.Context, body string) error {
	if _, _, err := c.gh.Issues.CreateComment(ctx, c.owner, c.repo, c.number, &github.IssueComment{Body: github.Ptr(body)}); err != nil {
		return fmt.Errorf("commenting on %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}
	return nil
}

// CommentsByAuthor returns the bodies of every comment login has posted on the
// pull request, review comments and conversation comments alike. It is how a
// retry tells whether it already answered a proposal: an interrupted job runs
// again, and the answer must not be posted twice.
func (c *Client) CommentsByAuthor(ctx context.Context, login string) ([]string, error) {
	if NormalizeLogin(login) == "" {
		return nil, fmt.Errorf("a login is required to look its comments up")
	}

	var bodies []string
	reviewOpts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		page, resp, err := c.gh.PullRequests.ListComments(ctx, c.owner, c.repo, c.number, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("listing review comments of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, comment := range page {
			if user := comment.GetUser(); user != nil && SameLogin(user.GetLogin(), login) {
				bodies = append(bodies, comment.GetBody())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}

	commentOpts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		page, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, c.number, commentOpts)
		if err != nil {
			return nil, fmt.Errorf("listing conversation comments of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, comment := range page {
			if user := comment.GetUser(); user != nil && SameLogin(user.GetLogin(), login) {
				bodies = append(bodies, comment.GetBody())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		commentOpts.Page = resp.NextPage
	}

	return bodies, nil
}

// ThreadCommentBy returns the body of the first comment in the chain that
// starts at commentID, inclusive, and was written by login. A caller passes the
// comment a command replied to, so the answer is Rumil's own finding in that
// thread. It returns "" when the chain holds nothing of Rumil's.
//
// The chain comes from in_reply_to, so it finds the comment however deep the
// reply sits. The comment the caller starts from is passed in rather than
// looked up, because the payload already names it and a listing that has not
// caught up would otherwise hide it.
func (c *Client) ThreadCommentBy(ctx context.Context, commentID int64, login string) (string, error) {
	comments, err := c.ExistingReviewComments(ctx)
	if err != nil {
		return "", err
	}

	byID := make(map[int64]ExistingComment, len(comments))
	for _, comment := range comments {
		byID[comment.ID] = comment
	}

	current := byID[commentID]
	for depth := 0; depth < threadDepthLimit && current.ID != 0; depth++ {
		if SameLogin(current.Author, login) {
			return current.Body, nil
		}
		current = byID[current.InReplyToID]
	}
	return "", nil
}
