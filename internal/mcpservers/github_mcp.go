// Package mcpservers exposes Rumil's MCP tools. The GitHub server is scoped to
// a single (owner, repo, pull request) session and never mutates branches,
// commits, or repository settings: the only write is a review.
package mcpservers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// GithubServerName is the MCP server name. Tool names are namespaced with it so
// a later memory server cannot collide with this one.
const GithubServerName = "github"

// Handler runs one tool call. A returned error is reported to the model as a
// tool error, so it can correct itself; a successful call returns the text
// payload.
type Handler func(ctx context.Context, args map[string]any) (string, error)

// Spec is a single tool definition: the one source of truth for the MCP server
// and for the agent session.
type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Handle      Handler
}

// GithubTools holds the session dependencies for the GitHub tool set.
type GithubTools struct {
	client       *ghapp.Client
	maxDiffBytes int
	logger       *slog.Logger

	// OnToolCall is called before each tool runs, so the caller can enforce and
	// report the session's turn budget.
	OnToolCall func(name string)

	// OnReviewSubmitted is called once a review reaches GitHub, so the caller
	// can record the outcome of the session.
	OnReviewSubmitted func(outcome ghapp.ReviewOutcome)
}

// NewGithubTools scopes the tools to one pull request.
func NewGithubTools(client *ghapp.Client, maxDiffBytes int, logger *slog.Logger) *GithubTools {
	return &GithubTools{client: client, maxDiffBytes: maxDiffBytes, logger: logger}
}

// Specs returns the tool set described in project_plan.md §5.1. Every tool is
// read-only except submit_pr_review.
func (t *GithubTools) Specs() []Spec {
	return []Spec{
		{
			Name:        "get_pr_metadata",
			Description: "Return the pull request title, description, author, base and head SHAs, state, and the changed files with their addition and deletion counts.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handle:      t.getMetadata,
		},
		{
			Name:        "get_pr_diff",
			Description: "Return the pull request diff annotated with explicit old and new line numbers in the form [OLD:12 NEW:15] + code. Pass file_path to limit the result to one file and page to walk large diffs.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Limit the diff to a single changed file path."},"page":{"type":"integer","description":"1-based page number of the annotated diff."}},"additionalProperties":false}`),
			Handle:      t.getDiff,
		},
		{
			Name:        "get_file_contents",
			Description: "Read a file from the repository at a git ref, defaulting to the pull request head SHA. Binary files are rejected and a single read is capped at 500 lines.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Repository-relative file path."},"ref":{"type":"string","description":"Git ref, defaults to the pull request head SHA."},"start_line":{"type":"integer","description":"First line to return, 1-based."},"end_line":{"type":"integer","description":"Last line to return, inclusive."}},"required":["file_path"],"additionalProperties":false}`),
			Handle:      t.getFileContents,
		},
		{
			Name:        "list_existing_review_comments",
			Description: "List the review comments already posted on this pull request so the same finding is never reported twice.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handle:      t.listExistingComments,
		},
		{
			Name:        "submit_pr_review",
			Description: "Submit the review. Inline comments must target lines that exist in the diff on the requested side; comments outside the diff are moved into the summary instead of failing. Existing identical comments are skipped, and a rejected APPROVE or REQUEST_CHANGES is resubmitted as COMMENT.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"event":{"type":"string","enum":["COMMENT","REQUEST_CHANGES","APPROVE"]},"summary_body":{"type":"string"},"comments":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"line":{"type":"integer"},"side":{"type":"string","enum":["RIGHT","LEFT"]},"body":{"type":"string"}},"required":["path","line","body"],"additionalProperties":false}}},"required":["event","summary_body"],"additionalProperties":false}`),
			Handle:      t.submitReview,
		},
	}
}

// Server builds the mcp-go server serving the specs above.
func (t *GithubTools) Server(version string) *server.MCPServer {
	srv := server.NewMCPServer(GithubServerName, version, server.WithToolCapabilities(true))

	for _, spec := range t.Specs() {
		spec := spec
		srv.AddTool(
			mcp.NewToolWithRawSchema(spec.Name, spec.Description, spec.Schema),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if t.OnToolCall != nil {
					t.OnToolCall(spec.Name)
				}
				text, err := spec.Handle(ctx, req.GetArguments())
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultText(text), nil
			},
		)
	}
	return srv
}

func (t *GithubTools) getMetadata(ctx context.Context, _ map[string]any) (string, error) {
	meta, err := t.client.Metadata(ctx)
	if err != nil {
		return "", fmt.Errorf("get_pr_metadata failed: %w", err)
	}

	type file struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	files := make([]file, 0, len(meta.ChangedFiles))
	for _, f := range meta.ChangedFiles {
		files = append(files, file{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
	}

	return marshalJSON(map[string]any{
		"number":        meta.Number,
		"title":         meta.Title,
		"body":          meta.Body,
		"author":        meta.Author,
		"state":         meta.State,
		"draft":         meta.Draft,
		"base_sha":      meta.BaseSHA,
		"head_sha":      meta.HeadSHA,
		"changed_files": files,
	})
}

func (t *GithubTools) getDiff(ctx context.Context, args map[string]any) (string, error) {
	filePath, _ := args["file_path"].(string)
	page := intArg(args, "page")

	diff, err := t.client.Diff(ctx)
	if err != nil {
		return "", fmt.Errorf("get_pr_diff failed: %w", err)
	}

	if filePath != "" {
		annotated := diff.Annotate(filePath)
		if annotated == "" {
			return "", fmt.Errorf("%s is not part of this pull request diff", filePath)
		}
		return annotated, nil
	}

	pages := diff.AnnotatedPages(t.maxDiffBytes)
	if page < 1 {
		page = 1
	}
	if page > len(pages) {
		return "", fmt.Errorf("page %d does not exist, the diff has %d pages", page, len(pages))
	}

	header := fmt.Sprintf("diff page %d of %d\n", page, len(pages))
	if len(pages) > 1 {
		header += "call get_pr_diff again with the next page number for the rest of the diff\n"
	}
	return header + pages[page-1], nil
}

func (t *GithubTools) getFileContents(ctx context.Context, args map[string]any) (string, error) {
	filePath, _ := args["file_path"].(string)
	ref, _ := args["ref"].(string)
	if filePath == "" {
		return "", fmt.Errorf("file_path is required")
	}

	content, err := t.client.FileContents(ctx, filePath, ref, intArg(args, "start_line"), intArg(args, "end_line"))
	if err != nil {
		return "", err
	}
	return content, nil
}

func (t *GithubTools) listExistingComments(ctx context.Context, _ map[string]any) (string, error) {
	comments, err := t.client.ExistingReviewComments(ctx)
	if err != nil {
		return "", fmt.Errorf("list_existing_review_comments failed: %w", err)
	}

	type comment struct {
		ID     int64  `json:"id"`
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	out := make([]comment, 0, len(comments))
	for _, c := range comments {
		out = append(out, comment{ID: c.ID, Path: c.Path, Line: c.Line, Author: c.Author, Body: c.Body})
	}
	return marshalJSON(out)
}

func (t *GithubTools) submitReview(ctx context.Context, args map[string]any) (string, error) {
	event, _ := args["event"].(string)
	summary, _ := args["summary_body"].(string)

	switch event {
	case ghapp.EventComment, ghapp.EventRequestChanges, ghapp.EventApprove:
	default:
		return "", fmt.Errorf("event must be COMMENT, REQUEST_CHANGES, or APPROVE, got %q", event)
	}

	rawComments, _ := args["comments"].([]any)
	comments := make([]ghapp.ReviewComment, 0, len(rawComments))
	for _, raw := range rawComments {
		entry, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("every comment must be an object with path, line, and body")
		}
		path, _ := entry["path"].(string)
		if path == "" {
			return "", fmt.Errorf("every comment needs a path")
		}
		line := intArg(entry, "line")
		if line < 1 {
			return "", fmt.Errorf("comment on %s needs a positive line number", path)
		}
		side, _ := entry["side"].(string)
		body, _ := entry["body"].(string)
		comments = append(comments, ghapp.ReviewComment{Path: path, Line: line, Side: side, Body: body})
	}

	diff, err := t.client.Diff(ctx)
	if err != nil {
		return "", fmt.Errorf("submit_pr_review failed: %w", err)
	}

	outcome, err := t.client.SubmitReview(ctx, diff, ghapp.ReviewRequest{
		Event:       event,
		SummaryBody: summary,
		Comments:    comments,
	})
	if err != nil {
		return "", fmt.Errorf("submit_pr_review failed: %w", err)
	}

	inline := len(comments) - len(outcome.MovedToSummary) - len(outcome.SkippedDuplicates)
	if t.OnReviewSubmitted != nil {
		t.OnReviewSubmitted(*outcome)
	}
	t.logger.Info("review submitted",
		"event", outcome.Event,
		"requested_event", outcome.RequestedEvent,
		"fallback", outcome.Fallback,
		"inline_comments", inline,
		"moved_to_summary", len(outcome.MovedToSummary),
		"skipped_duplicates", len(outcome.SkippedDuplicates),
	)

	return marshalJSON(map[string]any{
		"event":              outcome.Event,
		"requested_event":    outcome.RequestedEvent,
		"fallback":           outcome.Fallback,
		"inline_comments":    inline,
		"moved_to_summary":   outcome.MovedToSummary,
		"skipped_duplicates": outcome.SkippedDuplicates,
	})
}

// intArg reads an integer argument, accepting the JSON number types the MCP
// layer may hand over.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

func marshalJSON(value any) (string, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding tool result: %w", err)
	}
	return string(encoded), nil
}
