package ghapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MemoryCommentEditor rewrites the confirmation comments Rumil posted for a
// recorded rule. A confirmation names the rule's short id and its status at
// the moment it was posted, so once a maintainer approves or rejects the rule
// the comment keeps saying PENDING. The reviewer reads the pull request
// conversation back, so that stale line is what it repeats instead of using
// the approved rule.
type MemoryCommentEditor struct {
	auth *AppAuth
}

// NewMemoryCommentEditor scopes comment rewriting to the App that posted them.
func NewMemoryCommentEditor(auth *AppAuth) *MemoryCommentEditor {
	return &MemoryCommentEditor{auth: auth}
}

// RewriteMemoryComments replaces every confirmation comment on the pull
// request that names shortID and a status with body. Both review comments and
// conversation comments are considered, because a command is answered in its
// own thread and at the top level depending on where it arrived.
//
// The count is how many comments were rewritten. A failure to rewrite one
// comment does not stop the others; the errors are returned together so the
// caller can log them while keeping the moderation decision.
func (e *MemoryCommentEditor) RewriteMemoryComments(ctx context.Context, installationID int64, owner, repo string, number int, shortID, body string) (int, error) {
	if e == nil || e.auth == nil {
		return 0, errors.New("rewriting memory comments needs a configured GitHub App")
	}
	shortID = strings.TrimPrefix(strings.TrimSpace(shortID), "#")
	if shortID == "" {
		return 0, errors.New("rewriting memory comments needs the rule's short id")
	}
	if strings.TrimSpace(body) == "" {
		return 0, errors.New("rewriting memory comments needs a body")
	}

	record, err := e.auth.RecordFor(ctx, installationID)
	if err != nil {
		return 0, fmt.Errorf("reading the App's record: %w", err)
	}
	client, err := e.auth.ClientFor(installationID)
	if err != nil {
		return 0, err
	}
	comments := NewClient(client, owner, repo, number)

	// The marker is what distinguishes a confirmation from any other comment
	// that happens to mention the rule: every confirmation carries the status
	// it was posted with.
	needle := "#" + strings.ToLower(shortID)
	confirmation := func(author, text string) bool {
		return SameLogin(author, record.Slug) &&
			strings.Contains(strings.ToLower(text), needle) &&
			strings.Contains(strings.ToLower(text), "status:")
	}

	var (
		edited int
		errs   []error
	)

	reviewComments, err := comments.ExistingReviewComments(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, comment := range reviewComments {
		if !confirmation(comment.Author, comment.Body) {
			continue
		}
		if err := comments.EditReviewComment(ctx, comment.ID, body); err != nil {
			errs = append(errs, err)
			continue
		}
		edited++
	}

	conversation, err := comments.ConversationComments(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, comment := range conversation {
		if !confirmation(comment.Author, comment.Body) {
			continue
		}
		if err := comments.EditIssueComment(ctx, comment.ID, body); err != nil {
			errs = append(errs, err)
			continue
		}
		edited++
	}

	return edited, errors.Join(errs...)
}
