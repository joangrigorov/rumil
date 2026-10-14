package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/store"
)

// distillTurns bounds the synthesis session. Recording one rule takes a single
// tool call, so the cap is what stops a model that keeps talking.
const distillTurns = 4

// maxContextFiles bounds the file list a conversation comment is distilled
// against, so a pull request that touches thousands of files cannot fill the
// prompt with paths.
const maxContextFiles = 50

// RememberRequest is one @rumil remember command and the context it arrived in.
type RememberRequest struct {
	Owner  string
	Repo   string
	Number int
	// Command is the person's own words, the raw material of the rule.
	Command string
	// CommentID identifies the comment the answer goes under, when it is a
	// review-thread reply.
	CommentID int64
	// InReplyToID is the comment the command replied to, which is where the
	// thread walk for Rumil's own finding starts. Zero means the command opened
	// its own thread.
	InReplyToID int64
	// Conversation is true for a top-level comment, which carries no file and
	// no diff hunk.
	Conversation bool
	Path         string
	DiffHunk     string
	// RequestedBy and SourceComment record who asked, and where they asked.
	RequestedBy   string
	SourceComment string
}

// RememberResult reports what the synthesis session did.
type RememberResult struct {
	// Proposed is true when a rule reached the store.
	Proposed bool
	// Inserted is true when the rule was new rather than a wording of one
	// already recorded.
	Inserted bool
	// Memory is the stored row.
	Memory store.Memory
	// Replied is true when the confirmation reached the pull request.
	Replied bool
}

// ErrMemoryNotConfigured reports a command that arrived while memory is off.
// Memory is opt-in (project_plan.md §3 rule 6), so this is a configuration
// state rather than a fault.
var ErrMemoryNotConfigured = errors.New("memory is not configured")

// MemoryConfigured reports whether this engine can record and retrieve rules.
func (e *Engine) MemoryConfigured() bool { return e.memory != nil }

// Remember distills one @rumil remember command into a stored rule and answers
// the comment.
//
// The answer is posted by Go rather than by a tool: no tool set has a
// comment-creation member, and handing the model one would widen its write
// surface for nothing (project_plan.md §6 Phase 4).
func (e *Engine) Remember(ctx context.Context, req RememberRequest) (*RememberResult, error) {
	if e.memory == nil {
		return nil, fmt.Errorf("%w: set database.dsn and models.default_embedding to record rules", ErrMemoryNotConfigured)
	}
	if err := e.auth.Verify(ctx); err != nil {
		return nil, err
	}
	provider, err := e.cfg.Models.ChatProvider()
	if err != nil {
		return nil, fmt.Errorf("selecting the chat provider: %w", err)
	}

	client := ghapp.NewClient(e.auth.Client(), req.Owner, req.Repo, req.Number)

	// Rumil's own comment in the thread is the finding the person is answering,
	// so the rule is distilled against it. A thread Rumil never wrote in, and a
	// lookup that fails, both leave the context empty rather than stopping a
	// command the person is waiting on.
	prior := ""
	if !req.Conversation && req.InReplyToID != 0 {
		if body, err := client.ThreadCommentBy(ctx, req.InReplyToID, e.cfg.App.BotHandle); err != nil {
			e.logger.Warn("reading Rumil's comment in the thread failed", "error", err)
		} else {
			prior = body
		}
	}

	fileContext, err := rememberFileContext(ctx, client, req)
	if err != nil {
		return nil, err
	}

	result := &RememberResult{}
	memoryTools := mcpservers.NewMemoryTools(e.memory.store, e.memory.embedder, mcpservers.MemorySession{
		InstallationID: e.memory.installationID,
		OrgName:        req.Owner,
		RepoName:       req.Repo,
		PRNumber:       req.Number,
		SourceComment:  req.SourceComment,
		RequestedBy:    req.RequestedBy,
		AcceptAll:      e.cfg.Moderation.AcceptAll,
	}, e.logger)
	memoryTools.OnMemoryProposed = func(memory store.Memory, inserted bool) {
		if result.Proposed {
			e.logger.Warn("the synthesis session recorded a second rule, keeping the first",
				"first_memory", result.Memory.ID, "extra_memory", memory.ID)
			return
		}
		result.Proposed, result.Inserted, result.Memory = true, inserted, memory
	}

	sessionTools, err := kitTools(memoryTools.ProposeSpecs(), nil)
	if err != nil {
		return nil, fmt.Errorf("building the synthesis tools: %w", err)
	}

	session, err := kit.New(ctx, e.sessionOptions(provider, RememberPrompt(req, prior, fileContext), sessionTools, distillTurns))
	if err != nil {
		return nil, fmt.Errorf("starting the synthesis session: %w", err)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			e.logger.Warn("closing the synthesis session failed", "error", closeErr)
		}
	}()

	text, err := session.Prompt(ctx, RememberUserPrompt)
	if err != nil {
		return result, fmt.Errorf("running the synthesis session for %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
	}
	if !result.Proposed {
		e.logger.Warn("the synthesis session recorded no rule",
			"repo", req.Owner+"/"+req.Repo, "pr", req.Number, "requested_by", req.RequestedBy, "summary", text)
		return result, nil
	}

	answered, err := e.answeredWith(ctx, client, result.Memory.ShortID())
	if err != nil {
		// The check is what keeps a retry from answering twice, so an
		// inconclusive answer is left unanswered rather than posted blind.
		e.logger.Warn("could not tell whether the proposal was already answered, leaving the comment unanswered", "error", err)
		return result, nil
	}
	if answered {
		e.logger.Info("the proposal was already answered on the pull request", "memory", result.Memory.ID)
		return result, nil
	}

	e.logger.Info("recorded a rule from a comment",
		"memory", result.Memory.ID,
		"inserted", result.Inserted,
		"status", result.Memory.Status,
		"requested_by", req.RequestedBy,
	)

	reply := confirmationReply(result.Memory, result.Inserted)
	var replyErr error
	if req.Conversation || req.CommentID == 0 {
		replyErr = client.CreateIssueComment(ctx, reply)
	} else {
		replyErr = client.ReplyToReviewComment(ctx, req.CommentID, reply)
	}
	if replyErr != nil {
		return result, fmt.Errorf("answering the command: %w", replyErr)
	}
	result.Replied = true
	return result, nil
}

// answeredWith reports whether an earlier comment by Rumil already names this
// memory. An interrupted job returns to the queue and runs again, so this is
// what keeps the second run from posting the answer twice.
func (e *Engine) answeredWith(ctx context.Context, client *ghapp.Client, shortID string) (bool, error) {
	bodies, err := client.CommentsByAuthor(ctx, e.cfg.App.BotHandle)
	if err != nil {
		return false, err
	}
	needle := strings.ToLower("#" + shortID)
	for _, body := range bodies {
		if strings.Contains(strings.ToLower(body), needle) {
			return true, nil
		}
	}
	return false, nil
}

// confirmationReply is the answer posted under the command. It names the short
// id, so a maintainer can paste it into a moderation command, and the status,
// so the person knows whether the rule can be retrieved yet.
func confirmationReply(memory store.Memory, inserted bool) string {
	short := "#" + memory.ShortID()
	switch {
	case !inserted:
		return fmt.Sprintf("Memory `%s` already covers this (`status: %s`).", short, memory.Status)
	case memory.Status == store.MemoryApproved:
		return fmt.Sprintf("Memory `%s` recorded (`status: %s`).", short, memory.Status)
	default:
		return fmt.Sprintf("Memory proposal `%s` queued for maintainer moderation (`status: %s`).", short, memory.Status)
	}
}

// rememberFileContext renders the code the command is about: the hunk the
// comment hangs on, or, for a conversation comment, the files the pull request
// touches (project_plan.md §6 Phase 4).
func rememberFileContext(ctx context.Context, client *ghapp.Client, req RememberRequest) (string, error) {
	if req.Path != "" {
		return req.Path + "\n" + req.DiffHunk, nil
	}

	meta, err := client.Metadata(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the pull request metadata: %w", err)
	}
	paths := make([]string, 0, len(meta.ChangedFiles))
	for i, file := range meta.ChangedFiles {
		if i == maxContextFiles {
			paths = append(paths, "and more")
			break
		}
		paths = append(paths, file.Path)
	}
	return "the command is a conversation comment, so there is no hunk to read; the pull request changes " +
		strings.Join(paths, ", "), nil
}
