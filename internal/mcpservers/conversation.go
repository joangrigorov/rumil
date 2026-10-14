package mcpservers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// Bounds on the conversation view. Reviews are the narrative a later round
// reads first, so they are not part of the thread budget; a thread is dropped
// whole rather than cut in half.
const (
	conversationByteBudget = 32 << 10
	reviewBodyCap          = 4 << 10
	conversationPageMin    = 1
	conversationPageMax    = 100
	conversationPageSize   = 30
	// maxResolutionsPerReview bounds how many threads one review closes. A
	// review that closes everything at once is not reading its own findings.
	maxResolutionsPerReview = 20
)

// listConversation returns the pull request's review history in one view: every
// review, every thread with its resolution state and replies, and the
// conversation (project_plan.md §5.1).
func (t *GithubTools) listConversation(ctx context.Context, args map[string]any) (string, error) {
	includeResolved := true
	if value, ok := args["include_resolved"].(bool); ok {
		includeResolved = value
	}
	path, _ := args["path"].(string)
	before, _ := args["before"].(string)
	limit := intArg(args, "limit")
	switch {
	case limit < conversationPageMin:
		limit = conversationPageSize
	case limit > conversationPageMax:
		limit = conversationPageMax
	}

	reviews, err := t.client.Reviews(ctx)
	if err != nil {
		return "", fmt.Errorf("list_pr_conversation failed: %w", err)
	}
	threads, err := t.client.ReviewThreads(ctx)
	if err != nil {
		return "", fmt.Errorf("list_pr_conversation failed: %w", err)
	}
	conversation, err := t.client.ConversationComments(ctx)
	if err != nil {
		return "", fmt.Errorf("list_pr_conversation failed: %w", err)
	}

	page, omitted, next := t.threadPage(threads, includeResolved, path, before, limit)
	return marshalJSON(map[string]any{
		"reviews":         t.renderReviews(reviews),
		"threads":         page,
		"conversation":    renderConversation(conversation),
		"threads_omitted": omitted,
		"next_before":     next,
	})
}

// threadPage selects the threads to return: newest first, the ones the caller
// asked for, up to the limit and the byte budget. It returns how many threads
// were left out and the cursor that reads the next page.
func (t *GithubTools) threadPage(threads []ghapp.ReviewThread, includeResolved bool, path, before string, limit int) (page []map[string]any, omitted int, next string) {
	eligible := make([]ghapp.ReviewThread, 0, len(threads))
	for _, thread := range threads {
		if !includeResolved && thread.IsResolved {
			continue
		}
		if path != "" && thread.Path != path {
			continue
		}
		eligible = append(eligible, thread)
	}

	// Newest first: a later round cares most about what the last round said.
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].Newest().After(eligible[j].Newest())
	})

	start := 0
	if before != "" {
		for i, thread := range eligible {
			if thread.ID == before {
				start = i + 1
				break
			}
		}
	}

	page = make([]map[string]any, 0, limit)
	size := 0
	for _, thread := range eligible[start:] {
		if len(page) == limit {
			break
		}
		rendered := renderThread(thread)
		encoded, err := marshalJSON(rendered)
		if err != nil {
			continue
		}
		if size+len(encoded) > conversationByteBudget && len(page) > 0 {
			break
		}
		size += len(encoded)
		page = append(page, rendered)
	}

	remaining := len(eligible) - start - len(page)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 0 && len(page) > 0 {
		next, _ = page[len(page)-1]["id"].(string)
	}
	return page, remaining, next
}

func renderThread(thread ghapp.ReviewThread) map[string]any {
	comments := make([]map[string]any, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		comments = append(comments, map[string]any{
			"id":         comment.ID,
			"author":     comment.Author,
			"created_at": comment.CreatedAt.Format(time.RFC3339),
			"body":       comment.Body,
		})
	}
	rendered := map[string]any{
		"id":          thread.ID,
		"path":        thread.Path,
		"line":        thread.Line,
		"is_resolved": thread.IsResolved,
		"is_outdated": thread.IsOutdated,
		"comments":    comments,
	}
	if thread.HasMoreComments {
		rendered["has_more_comments"] = true
	}
	return rendered
}

// renderReviews lists the reviews oldest first. Rumil's own summaries are the
// narrative spine, so they are kept whole; another author's long body is
// trimmed, because the model needs to know a review happened more than it needs
// every word of it.
func (t *GithubTools) renderReviews(reviews []ghapp.Review) []map[string]any {
	rendered := make([]map[string]any, 0, len(reviews))
	for _, review := range reviews {
		body := review.Body
		if !t.isBot(review.Author) && len(body) > reviewBodyCap {
			body = body[:reviewBodyCap] + "\n[trimmed]"
		}
		entry := map[string]any{
			"id":      review.ID,
			"author":  review.Author,
			"state":   review.State,
			"commit":  review.CommitID,
			"body":    body,
			"is_mine": t.isBot(review.Author),
		}
		if !review.SubmittedAt.IsZero() {
			entry["submitted_at"] = review.SubmittedAt.Format(time.RFC3339)
		}
		rendered = append(rendered, entry)
	}
	return rendered
}

func renderConversation(comments []ghapp.ConversationComment) []map[string]any {
	rendered := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		entry := map[string]any{
			"id":     comment.ID,
			"author": comment.Author,
			"body":   comment.Body,
		}
		if !comment.CreatedAt.IsZero() {
			entry["created_at"] = comment.CreatedAt.Format(time.RFC3339)
		}
		rendered = append(rendered, entry)
	}
	return rendered
}

// resolveThread closes one of Rumil's own threads once the current code shows
// the finding is gone, and posts the reason into the thread (T-020-08).
func (t *GithubTools) resolveThread(ctx context.Context, args map[string]any) (string, error) {
	threadID, _ := args["thread_id"].(string)
	reason, _ := args["reason"].(string)
	if threadID == "" {
		return "", fmt.Errorf("thread_id is required")
	}
	if reason == "" {
		return "", fmt.Errorf("reason is required")
	}

	own, err := t.ownReviewCount(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve_review_thread failed: %w", err)
	}
	// A first pass leaves its findings open for the author to answer; closing
	// threads starts with the second review of a pull request.
	if own < 1 {
		return "", fmt.Errorf("closing threads starts with the second review of a pull request, and this is the first")
	}

	threads, err := t.client.ReviewThreads(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve_review_thread failed: %w", err)
	}
	thread, found := findThread(threads, threadID)
	if !found {
		return "", fmt.Errorf("no review thread %s on this pull request", threadID)
	}
	if thread.IsResolved {
		return "", fmt.Errorf("thread %s is already resolved", threadID)
	}
	root, ok := thread.Root()
	if !ok {
		return "", fmt.Errorf("thread %s has no comments", threadID)
	}
	if !t.isBot(root.Author) {
		return "", fmt.Errorf("thread %s was opened by %s, and only Rumil's own threads can be closed", threadID, root.Author)
	}

	if closed := t.resolutionCount(); closed >= maxResolutionsPerReview {
		return "", fmt.Errorf("this review has already closed %d threads, which is the cap", closed)
	}

	// The reason goes in first: a resolution whose reason failed to post would
	// be a silent one, and an open thread with an answer in it is recoverable.
	if err := t.client.ReplyToReviewComment(ctx, root.ID, reason); err != nil {
		return "", fmt.Errorf("resolve_review_thread failed: %w", err)
	}
	if err := t.client.ResolveReviewThread(ctx, threadID); err != nil {
		return "", fmt.Errorf("resolve_review_thread failed: %w", err)
	}
	closed := t.countResolution()

	t.logger.Info("resolved a review thread",
		"thread_id", threadID, "path", thread.Path, "line", thread.Line, "closed_this_review", closed,
	)
	return marshalJSON(map[string]any{
		"thread_id": threadID,
		"resolved":  true,
		"reason":    reason,
		"closed":    closed,
		"cap":       maxResolutionsPerReview,
	})
}

// isBot reports whether a login is Rumil's own.
func (t *GithubTools) isBot(login string) bool {
	return ghapp.SameLogin(login, t.botLogin)
}

// ownReviewCount is how many reviews Rumil has already submitted on this pull
// request, read once per session because closing a thread cannot change it.
func (t *GithubTools) ownReviewCount(ctx context.Context) (int, error) {
	reviews, err := t.readReviews(ctx)
	if err != nil {
		return 0, err
	}
	own := 0
	for _, review := range reviews {
		if t.isBot(review.Author) {
			own++
		}
	}
	return own, nil
}

// readReviews is the session's cached view of the reviews, which the history
// view, the metadata tool and the resolution rule all read.
func (t *GithubTools) readReviews(ctx context.Context) ([]ghapp.Review, error) {
	t.reviewsMu.Lock()
	defer t.reviewsMu.Unlock()
	if t.reviewsRead {
		return t.reviews, nil
	}

	reviews, err := t.client.Reviews(ctx)
	if err != nil {
		return nil, err
	}
	t.reviews, t.reviewsRead = reviews, true
	return reviews, nil
}

func (t *GithubTools) resolutionCount() int {
	t.resolutionsMu.Lock()
	defer t.resolutionsMu.Unlock()
	return t.resolutions
}

func (t *GithubTools) countResolution() int {
	t.resolutionsMu.Lock()
	defer t.resolutionsMu.Unlock()
	t.resolutions++
	return t.resolutions
}

func findThread(threads []ghapp.ReviewThread, id string) (ghapp.ReviewThread, bool) {
	for _, thread := range threads {
		if thread.ID == id {
			return thread, true
		}
	}
	return ghapp.ReviewThread{}, false
}
