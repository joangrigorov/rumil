package reviewer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// checkRunTimeout bounds the call that completes a check run. It runs after the
// review's own context, which may already be expired, so it gets a fresh one.
const checkRunTimeout = 10 * time.Second

// startReviewCheck opens the check run for the commit under review. A failure
// is logged and the review continues: the check is progress, not the work.
func (e *Engine) startReviewCheck(ctx context.Context, req Request, headSHA string) int64 {
	if headSHA == "" {
		return 0
	}
	authClient, err := e.auth.ClientFor(req.InstallationID)
	if err != nil {
		e.logger.Warn("starting the review check run failed, the review continues without it", "error", err)
		return 0
	}
	client := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)
	id, err := client.StartReviewCheck(ctx, headSHA, e.checkDetailsURL(req.SessionID), e.cfg.GitHub.AppID)
	if err != nil {
		e.logger.Warn("starting the review check run failed, the review continues without it", "error", err)
		return 0
	}
	return id
}

// finishReviewCheck completes the check run with the outcome of the review. It
// cannot reuse the review's context: a timeout is one of the outcomes being
// reported, so the call runs on a context that survives it.
func (e *Engine) finishReviewCheck(parent context.Context, req Request, id int64, result *Result, reviewErr error) {
	if id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), checkRunTimeout)
	defer cancel()

	conclusion, summary := checkConclusion(result, reviewErr)
	authClient, err := e.auth.ClientFor(req.InstallationID)
	if err != nil {
		e.logger.Warn("completing the review check run failed", "error", err)
		return
	}
	client := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)
	if err := client.FinishReviewCheck(ctx, id, conclusion, summary); err != nil {
		e.logger.Warn("completing the review check run failed", "error", err)
	}
}

// checkDetailsURL is the session page a reviewer can open from the check. It is
// empty when the operator has not named a public address, because an internal
// listener is not a link a browser on github.com can follow.
func (e *Engine) checkDetailsURL(sessionID string) string {
	base := strings.TrimRight(e.cfg.Server.PublicURL, "/")
	if base == "" || sessionID == "" {
		return ""
	}
	return base + "/ui/sessions/" + url.PathEscape(sessionID)
}

// checkConclusion maps the review's outcome onto the check run vocabulary. A
// comment-only review is neutral so it does not turn into a red check under
// branch protection; requested changes are action_required, which is the
// closest state to "a person has to act".
func checkConclusion(result *Result, reviewErr error) (conclusion, summary string) {
	switch {
	case errors.Is(reviewErr, ErrReviewCancelled):
		return "cancelled", "The review was cancelled."
	case errors.Is(reviewErr, context.Canceled):
		return "cancelled", "The review was cancelled."
	case reviewErr != nil:
		return "failure", reviewErr.Error()
	case result == nil || !result.ReviewSubmitted:
		return "failure", "The review ended without submitting a review."
	case result.Event == ghapp.EventApprove:
		return "success", fmt.Sprintf("Approved after %d tool calls.", result.ToolCalls)
	case result.Event == ghapp.EventRequestChanges:
		return "action_required", fmt.Sprintf("Changes requested after %d tool calls.", result.ToolCalls)
	default:
		return "neutral", fmt.Sprintf("Review submitted with comments after %d tool calls.", result.ToolCalls)
	}
}
