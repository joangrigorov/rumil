package ghapp

import (
	"context"
	"fmt"
	"sort"

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

// ConversationCommentBodies returns the bodies of the conversation comments
// login has posted on the pull request. It is how a retry tells whether it
// already answered a top-level command: an interrupted job runs again, and the
// answer must not be posted twice.
func (c *Client) ConversationCommentBodies(ctx context.Context, login string) ([]string, error) {
	if NormalizeLogin(login) == "" {
		return nil, fmt.Errorf("a login is required to look its comments up")
	}

	var bodies []string
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

// ThreadCommentBodies returns the bodies of login's comments that answer
// commentID: the comments whose in_reply_to chain passes through it. A command
// is answered inside its own thread, so this is the view that tells whether
// that particular command already has its answer; every other thread on the
// pull request is a different question.
func (c *Client) ThreadCommentBodies(ctx context.Context, commentID int64, login string) ([]string, error) {
	if NormalizeLogin(login) == "" {
		return nil, fmt.Errorf("a login is required to look its comments up")
	}
	comments, err := c.ExistingReviewComments(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]ExistingComment, len(comments))
	for _, comment := range comments {
		byID[comment.ID] = comment
	}

	var bodies []string
	for _, comment := range comments {
		if SameLogin(comment.Author, login) && repliesTo(byID, comment.ID, commentID) {
			bodies = append(bodies, comment.Body)
		}
	}
	return bodies, nil
}

// repliesTo reports whether the in_reply_to chain that starts at id passes
// through ancestor. The command comment is normally in the listing, but it does
// not have to be: a reply that names it as its parent is already in the same
// thread.
func repliesTo(byID map[int64]ExistingComment, id, ancestor int64) bool {
	for depth := 0; depth < threadDepthLimit; depth++ {
		if id == ancestor {
			return true
		}
		if id == 0 {
			return false
		}
		id = byID[id].InReplyToID
	}
	return false
}

// Thread returns the comments of the review thread commentID belongs to,
// oldest first. The walk goes up through in_reply_to to the comment that
// opened the thread and then collects everything hanging below it, which is
// what gives an answer the finding it explains and the exchange since.
//
// A comment the listing has not caught up with yields an empty thread rather
// than an error: the caller still has the payload's own body and hunk.
func (c *Client) Thread(ctx context.Context, commentID int64) ([]ExistingComment, error) {
	comments, err := c.ExistingReviewComments(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]ExistingComment, len(comments))
	for _, comment := range comments {
		byID[comment.ID] = comment
	}

	root := commentID
	for depth := 0; depth < threadDepthLimit; depth++ {
		comment, ok := byID[root]
		if !ok || comment.InReplyToID == 0 {
			break
		}
		root = comment.InReplyToID
	}

	thread := make([]ExistingComment, 0, len(comments))
	for _, comment := range comments {
		if repliesTo(byID, comment.ID, root) {
			thread = append(thread, comment)
		}
	}
	sort.SliceStable(thread, func(i, j int) bool {
		if thread[i].CreatedAt.Equal(thread[j].CreatedAt) {
			return thread[i].ID < thread[j].ID
		}
		return thread[i].CreatedAt.Before(thread[j].CreatedAt)
	})
	return thread, nil
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
