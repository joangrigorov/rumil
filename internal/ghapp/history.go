package ghapp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v69/github"
)

// Review is one review submitted on the pull request.
type Review struct {
	ID          int64
	Author      string
	State       string
	Body        string
	CommitID    string
	SubmittedAt time.Time
}

// ConversationComment is one top-level comment on the pull request, outside any
// review thread.
type ConversationComment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// Reviews returns every review the pull request has received, oldest first.
func (c *Client) Reviews(ctx context.Context) ([]Review, error) {
	var reviews []Review
	opts := &github.ListOptions{PerPage: 100}

	for {
		page, resp, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, c.number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing reviews of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, review := range page {
			entry := Review{
				ID:          review.GetID(),
				State:       review.GetState(),
				Body:        review.GetBody(),
				CommitID:    review.GetCommitID(),
				SubmittedAt: review.GetSubmittedAt().Time,
			}
			if user := review.GetUser(); user != nil {
				entry.Author = user.GetLogin()
			}
			reviews = append(reviews, entry)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return reviews, nil
}

// LastReviewedCommit returns the commit the most recent review by login was
// submitted against, which is where a later round measures its delta from. It
// returns "" when login has never reviewed the pull request.
func (c *Client) LastReviewedCommit(ctx context.Context, login string) (string, error) {
	reviews, err := c.Reviews(ctx)
	if err != nil {
		return "", err
	}
	return LastReviewedCommitOf(reviews, login), nil
}

// LastReviewedCommitOf is the same question asked of a review list a caller
// already holds, so a session that reads the history does not read it twice.
func LastReviewedCommitOf(reviews []Review, login string) string {
	commit := ""
	newest := time.Time{}
	for _, review := range reviews {
		if !SameLogin(review.Author, login) {
			continue
		}
		// The listing is oldest first, so the last one wins; the timestamp is
		// only used to keep a fixture that claims otherwise from winning.
		if review.SubmittedAt.Before(newest) {
			continue
		}
		newest = review.SubmittedAt
		commit = review.CommitID
	}
	return commit
}

// ConversationComments returns the pull request's top-level comments, oldest
// first.
func (c *Client) ConversationComments(ctx context.Context) ([]ConversationComment, error) {
	var comments []ConversationComment
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		page, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, c.number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing conversation comments of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, comment := range page {
			entry := ConversationComment{
				ID:        comment.GetID(),
				Body:      comment.GetBody(),
				CreatedAt: comment.GetCreatedAt().Time,
			}
			if user := comment.GetUser(); user != nil {
				entry.Author = user.GetLogin()
			}
			comments = append(comments, entry)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return comments, nil
}

// DiffSince returns the changes between base and head, which is how a later
// round reads only what has been pushed since its last review. An empty diff
// means nothing changed, and GitHub's push diff goes through this rather than
// the comment one.
func (c *Client) DiffSince(ctx context.Context, base, head string) (*Diff, error) {
	if base == "" || head == "" {
		return nil, fmt.Errorf("both the base and the head commit are required to read a partial diff")
	}

	raw, _, err := c.gh.Repositories.CompareCommitsRaw(ctx, c.owner, c.repo, base, head, github.RawOptions{Type: github.Diff})
	if err != nil {
		return nil, fmt.Errorf("comparing %s...%s in %s/%s: %w", base, head, c.owner, c.repo, err)
	}

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing the comparison %s...%s: %w", base, head, err)
	}
	return parsed, nil
}
