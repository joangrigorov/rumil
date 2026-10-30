package reviewer

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// maxAnswerBytes bounds the answer posted into a thread. A question is short,
// so an answer long enough to be useful fits well inside it; the cap keeps one
// reply from crowding the thread and one session from spending an unbounded
// output budget.
const maxAnswerBytes = 4000

// answerContextLines is how many lines around the commented line the answer
// session reads. The hunk itself is already in the payload; the excerpt is for
// the context the finding does not show.
const answerContextLines = 20

// answerTurns bounds the answer session. An answer is one model call and one
// reply, so a second step would be a model that did not stop talking.
const answerTurns = 1

// AnswerRequest is one @rumil why or @rumil explain command.
type AnswerRequest struct {
	Owner  string
	Repo   string
	Number int
	// InstallationID is the installation the command arrived through. Zero
	// uses the primary installation.
	InstallationID int64
	// CommentID is the command's own comment, which the answer replies to and
	// which the marker on the answer names.
	CommentID int64
	// InReplyToID is the comment the command replied to, and zero when the
	// command opened the thread. It is where the thread walk starts.
	InReplyToID int64
	// Question is what the person wrote after the verb, capped by the parser.
	// It is empty when the command carries no words of its own.
	Question string
	// RequestedBy is the commenter's login.
	RequestedBy string
	// Path, Line and DiffHunk are the finding's place: the file the comment
	// hangs on, its line in the reviewed diff, and the hunk GitHub sent with
	// the delivery.
	Path     string
	Line     int
	DiffHunk string
}

// AnswerResult reports what the answer session did.
type AnswerResult struct {
	// Replied is true when the answer reached the thread.
	Replied bool
	// Answer is the text that was posted, without the retry marker.
	Answer string
}

// Answer explains a finding inside its review thread.
//
// It is one model call and one reply: the session has no tools, so nothing it
// says can change the pull request, and the answer is posted by Go rather than
// by a tool. A question whose thread Rumil never wrote in is answered from the
// thread's own context, and the prompt says not to claim the finding.
func (e *Engine) Answer(ctx context.Context, req AnswerRequest) (*AnswerResult, error) {
	installationID, err := e.resolveInstallation(ctx, req.InstallationID, req.Owner, req.Repo)
	if err != nil {
		return nil, err
	}
	req.InstallationID = installationID
	if err := e.auth.VerifyInstallation(ctx, installationID); err != nil {
		return nil, err
	}
	provider, err := e.chatProviderFor(installationID, "")
	if err != nil {
		return nil, fmt.Errorf("selecting the chat provider: %w", err)
	}
	authClient, err := e.auth.ClientFor(installationID)
	if err != nil {
		return nil, err
	}

	client := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)

	// An interrupted job returns to the queue and runs again. The marker is what
	// stops the second run from answering the same comment twice; a check that
	// cannot be read is left unanswered rather than posted blind, because a
	// duplicate costs the person a notification and the operator a model call.
	answered, err := e.answerAlreadyPosted(ctx, client, req.CommentID, e.cfg.App.BotHandle)
	if err != nil {
		e.logger.Warn("could not tell whether the question was already answered, leaving it unanswered", "error", err)
		return &AnswerResult{}, nil
	}
	if answered {
		e.logger.Info("this question was already answered on the pull request", "comment_id", req.CommentID)
		return &AnswerResult{}, nil
	}

	thread, err := client.Thread(ctx, answerThreadStart(req))
	if err != nil {
		// The payload already carries the finding's own body and hunk, so a
		// listing that cannot be read narrows the answer instead of stopping it.
		e.logger.Warn("reading the review thread failed", "error", err)
		thread = nil
	}
	rootIsRumil := len(thread) > 0 && ghapp.SameLogin(thread[0].Author, e.cfg.App.BotHandle)
	thread = withoutComment(thread, req.CommentID)

	code, err := e.answerCode(ctx, client, req)
	if err != nil {
		e.logger.Warn("reading the code the question is about failed", "error", err)
	}

	systemPrompt := AnswerPrompt(req, thread, code, rootIsRumil, e.cfg.App.BotHandle)
	session, err := kit.New(ctx, e.sessionOptions(provider, systemPrompt, nil, answerTurns, false))
	if err != nil {
		return nil, fmt.Errorf("starting the answer session: %w", err)
	}
	stopCompaction := e.watchContext(ctx, session)
	defer stopCompaction()
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			e.logger.Warn("closing the answer session failed", "error", closeErr)
		}
	}()

	text, err := session.Prompt(ctx, AnswerUserPrompt)
	if err != nil {
		return nil, fmt.Errorf("running the answer session for %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
	}
	answer := capAnswer(text)
	if answer == "" {
		return nil, fmt.Errorf("the answer session for %s/%s#%d returned no text", req.Owner, req.Repo, req.Number)
	}

	if err := client.ReplyToReviewComment(ctx, req.CommentID, answerBody(answer, req.CommentID)); err != nil {
		return nil, fmt.Errorf("answering the question: %w", err)
	}
	e.logger.Info("answered a question in a review thread",
		"comment_id", req.CommentID,
		"path", req.Path,
		"requested_by", req.RequestedBy,
		"root_is_rumil", rootIsRumil,
	)
	return &AnswerResult{Replied: true, Answer: answer}, nil
}

// answerThreadStart is where the thread walk begins: the comment the command
// replied to when there is one, so the walk does not depend on the listing
// having caught up with the command itself.
func answerThreadStart(req AnswerRequest) int64 {
	if req.InReplyToID != 0 {
		return req.InReplyToID
	}
	return req.CommentID
}

// withoutComment drops the command's own comment from the thread it is
// rendered in. Its text is the question, and the prompt carries that once.
func withoutComment(thread []ghapp.ExistingComment, commentID int64) []ghapp.ExistingComment {
	out := thread[:0]
	for _, comment := range thread {
		if comment.ID == commentID {
			continue
		}
		out = append(out, comment)
	}
	return out
}

// answerCode renders the code the answer rests on: the hunk GitHub sent, then
// the file around the commented line at the reviewed commit.
func (e *Engine) answerCode(ctx context.Context, client *ghapp.Client, req AnswerRequest) (string, error) {
	var b strings.Builder
	if req.DiffHunk != "" {
		b.WriteString("<diff_hunk>\n")
		b.WriteString(neuterTag(req.DiffHunk, "diff_hunk"))
		b.WriteString("\n</diff_hunk>\n")
	}
	if req.Path == "" {
		return b.String(), nil
	}

	meta, err := client.Metadata(ctx)
	if err != nil {
		return b.String(), fmt.Errorf("reading the pull request metadata: %w", err)
	}
	if meta.HeadSHA == "" {
		return b.String(), nil
	}

	start, end := 1, 0
	if req.Line > 0 {
		start = req.Line - answerContextLines
		if start < 1 {
			start = 1
		}
		end = req.Line + answerContextLines
	}
	text, err := client.FileContents(ctx, req.Path, meta.HeadSHA, start, end)
	if err != nil {
		return b.String(), err
	}
	fmt.Fprintf(&b, "<file_excerpt path=%q from_line=\"%d\" commit=%q>\n", req.Path, start, meta.HeadSHA)
	b.WriteString(neuterTag(text, "file_excerpt"))
	b.WriteString("\n</file_excerpt>\n")
	return b.String(), nil
}

// answerAlreadyPosted reports whether this comment already has Rumil's answer,
// which is what keeps an interrupted job's second run from posting it twice.
func (e *Engine) answerAlreadyPosted(ctx context.Context, client *ghapp.Client, commentID int64, login string) (bool, error) {
	bodies, err := client.ThreadCommentBodies(ctx, commentID, login)
	if err != nil {
		return false, err
	}
	marker := answerMarker(commentID)
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// capAnswer trims the model's text and cuts it at the length cap, on a rune
// boundary, so a long answer is visibly shortened rather than silently
// truncated.
func capAnswer(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxAnswerBytes {
		return text
	}
	cut := maxAnswerBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "\n\n[the answer was cut at the length limit]"
}

// answerMarker is the invisible line that names the comment an answer belongs
// to. GitHub renders it as nothing, and the retry check finds it.
func answerMarker(commentID int64) string {
	return fmt.Sprintf("<!-- rumil-answer:%d -->", commentID)
}

// answerBody is what is posted: the answer, then the marker that makes it
// findable on a retry.
func answerBody(answer string, commentID int64) string {
	return answer + "\n\n" + answerMarker(commentID)
}
