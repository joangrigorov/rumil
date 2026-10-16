package mcpservers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// Bounds on the conversation view. The byte budget is what keeps one long
// thread or comment from filling the model's context; the page size is what
// keeps one call from returning a whole pull request's history.
const (
	conversationByteBudget = 32 << 10
	reviewBodyCap          = 4 << 10
	conversationBodyCap    = 4 << 10
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

	reviews, err := t.readReviews(ctx)
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

	page, omitted, next, err := t.threadPage(threads, includeResolved, path, before, limit)
	if err != nil {
		return "", fmt.Errorf("list_pr_conversation failed: %w", err)
	}
	// The threads page owns the budget it filled; the conversation gets what is
	// left, newest first, so the tail of a long discussion still arrives.
	comments, commentsOmitted := renderConversation(conversation, conversationByteBudget-pageBytes(page))
	return marshalJSON(map[string]any{
		"reviews":              t.renderReviews(reviews),
		"threads":              page,
		"conversation":         comments,
		"threads_omitted":      omitted,
		"conversation_omitted": commentsOmitted,
		"next_before":          next,
	})
}

// pageBytes is the encoded size of a rendered thread page.
func pageBytes(page []map[string]any) int {
	size := 0
	for _, thread := range page {
		encoded, err := marshalJSON(thread)
		if err != nil {
			continue
		}
		size += len(encoded)
	}
	return size
}

// threadPage selects the threads to return: newest first, the ones the caller
// asked for, up to the limit and the byte budget. It returns how many threads
// were left out and the cursor that reads the next page. A cursor the filters
// exclude is an error rather than a silent restart at the top of the list,
// which would leave a caller walking next_before in a loop.
func (t *GithubTools) threadPage(threads []ghapp.ReviewThread, includeResolved bool, path, before string, limit int) (page []map[string]any, omitted int, next string, err error) {
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
		found := false
		for i, thread := range eligible {
			if thread.ID == before {
				start, found = i+1, true
				break
			}
		}
		if !found {
			return nil, 0, "", fmt.Errorf("no thread %s in this view: it is missing, already returned, or excluded by include_resolved and path", before)
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
	return page, remaining, next, nil
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
// trimmed, and every body is capped, because a review body is text someone else
// wrote (T-020-07).
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

// renderConversation renders the conversation comments, oldest first, keeping
// the newest that fit the budget and reporting how many older ones were left
// out: a long discussion is read from its tail, which is where the answer to
// the current round usually is.
func renderConversation(comments []ghapp.ConversationComment, budget int) ([]map[string]any, int) {
	kept := make([]map[string]any, 0, len(comments))
	size := 0
	for i := len(comments) - 1; i >= 0; i-- {
		entry := renderConversationComment(comments[i])
		encoded, err := marshalJSON(entry)
		if err != nil {
			continue
		}
		if size+len(encoded) > budget && len(kept) > 0 {
			break
		}
		size += len(encoded)
		kept = append(kept, entry)
	}

	rendered := make([]map[string]any, 0, len(kept))
	for i := len(kept) - 1; i >= 0; i-- {
		rendered = append(rendered, kept[i])
	}
	return rendered, len(comments) - len(rendered)
}

func renderConversationComment(comment ghapp.ConversationComment) map[string]any {
	body := comment.Body
	if len(body) > conversationBodyCap {
		body = body[:conversationBodyCap] + "\n[trimmed]"
	}
	entry := map[string]any{
		"id":     comment.ID,
		"author": comment.Author,
		"body":   body,
	}
	if !comment.CreatedAt.IsZero() {
		entry["created_at"] = comment.CreatedAt.Format(time.RFC3339)
	}
	return entry
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
		if errors.Is(err, ghapp.ErrThreadResolveUnsupported) {
			// The reason is already in the thread, so whoever reads it sees why
			// the finding is gone even though GitHub will not let this App close
			// it. Calling again would only post a second one.
			return "", fmt.Errorf("resolve_review_thread cannot close a thread for this App: %w. Closing a review thread needs the App's Contents permission to be read and write, whatever its Pull requests permission says, and `rumil serve` reports the level at startup. The reason you posted is in the thread; say in the review summary which threads the commits have fixed instead of calling this tool again", err)
		}
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

// ownReviewCount is how many reviews Rumil had already submitted when this
// session started. It is read from the baseline, so a review this session
// submits is not counted: otherwise submitting first would make a first pass
// look like a second one.
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

// readReviews is the session's baseline view of the reviews: the history view,
// the metadata tool and the resolution guard all read it, so they agree on
// which rounds happened before this session (see NewGithubTools).
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
