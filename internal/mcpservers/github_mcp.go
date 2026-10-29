// Package mcpservers defines Rumil's tool set. The GitHub tools are scoped to a
// single (owner, repo, pull request) session and never mutate branches,
// commits, or repository settings: the only write is a review.
//
// A Spec is the one definition of a tool. The review session registers the
// specs as kit raw tools, so a call is a direct in-process Go call. The mcp-go
// server that publishes the same specs over stdio for external clients arrives
// with `rumil mcp`.
package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/snapshot"
)

// Handler runs one tool call. A returned error is reported to the model as a
// tool error, so it can correct itself; a successful call returns the text
// payload.
type Handler func(ctx context.Context, args map[string]any) (string, error)

// Spec is a single tool definition: the one source of truth for the agent
// session and, later, for the stdio MCP server.
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
	botLogin     string
	logger       *slog.Logger

	// snap is the session's working tree, nil when the snapshot is off or the
	// download failed. It backs the local read and search tools; the GitHub
	// API remains the fallback for listing and for other refs.
	snap *snapshot.Snapshot
	// headSHA is the commit under review, the default ref of every read.
	headSHA string

	// OnReviewSubmitted is called once a review reaches GitHub, so the caller
	// can record the outcome of the session.
	OnReviewSubmitted func(outcome ghapp.ReviewOutcome)

	// reviews is the session's view of the review list: the baseline the
	// history view and the resolution guard read.
	reviewsMu   sync.Mutex
	reviews     []ghapp.Review
	reviewsRead bool

	// resolutions counts the threads this review has closed, so the per-review
	// cap holds whatever the model does with the tool.
	resolutionsMu sync.Mutex
	resolutions   int
}

// GithubToolsOptions carries what one session's tool set is scoped to.
type GithubToolsOptions struct {
	MaxDiffBytes int
	BotHandle    string
	Logger       *slog.Logger
	// PriorReviews is the review list as it stood when the session was
	// created; see NewGithubTools.
	PriorReviews []ghapp.Review
	// Snapshot, when set, is the working tree at HeadSHA. It makes listing,
	// searching and reading local.
	Snapshot *snapshot.Snapshot
	// HeadSHA is the commit under review.
	HeadSHA string
}

// NewGithubTools scopes the tools to one pull request. botHandle is the
// configured handle, which is how a review recognises its own comments.
//
// priorReviews is the review list as it stood when the session was created, and
// it is the session's baseline rather than a cache: the metadata tool reports
// the last commit an earlier round reviewed against it, and the resolution
// guard counts the rounds that happened before this session. A session that
// read the list lazily could count the review it just submitted as an earlier
// round and close the threads it had only just opened, which is why the
// baseline is an argument and not something the tool set fills in on first use.
// A nil slice is a session whose history could not be read; the resolution
// guard then counts no earlier round, which is the safe direction.
func NewGithubTools(client *ghapp.Client, opts GithubToolsOptions) *GithubTools {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &GithubTools{
		client:       client,
		maxDiffBytes: opts.MaxDiffBytes,
		botLogin:     opts.BotHandle,
		logger:       logger,
		reviews:      opts.PriorReviews,
		reviewsRead:  true,
		snap:         opts.Snapshot,
		headSHA:      opts.HeadSHA,
	}
}

// Specs returns the tool set described in project_plan.md §5.1. The reads are
// diff, file, tree, history; the writes are the review and, from a pull
// request's second review on, closing Rumil's own threads with a reason.
//
// search_code is present only when the session has a working tree: without one
// there is nothing to search locally, and GitHub's code search reads the
// default branch rather than the commit under review, so advertising it would
// invite answers about different code.
func (t *GithubTools) Specs() []Spec {
	specs := []Spec{
		{
			Name:        "get_pr_metadata",
			Description: "Return the pull request title, description, author, base and head SHAs, state, the changed files with their addition and deletion counts, and last_reviewed_commit: the SHA the most recent review was submitted against, empty when this pull request has not been reviewed yet.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handle:      t.getMetadata,
		},
		{
			Name:        "get_pr_diff",
			Description: "Return the pull request diff annotated with explicit old and new line numbers in the form [OLD:12 NEW:15] + code. Pass file_path to limit the result to one file, since to read only the commits pushed after that SHA, and page to walk large diffs.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Limit the diff to a single changed file path."},"since":{"type":"string","description":"A commit SHA; the result holds only what changed after it, which is how a re-review reads its delta."},"page":{"type":"integer","description":"1-based page number of the annotated diff."}},"additionalProperties":false}`),
			Handle:      t.getDiff,
		},
		{
			Name:        "get_file_contents",
			Description: "Read a file from the repository at a git ref, defaulting to the pull request head SHA. Binary files are rejected and a single read is capped at 500 lines.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Repository-relative file path."},"ref":{"type":"string","description":"Git ref, defaults to the pull request head SHA."},"start_line":{"type":"integer","description":"First line to return, 1-based."},"end_line":{"type":"integer","description":"Last line to return, inclusive."}},"required":["file_path"],"additionalProperties":false}`),
			Handle:      t.getFileContents,
		},
		{
			Name:        "list_repository_tree",
			Description: "List file paths in the repository at the reviewed commit. Filter with prefix to stay inside a directory and with glob to match a file name such as *.go, then read only the files a finding needs instead of guessing a path.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"prefix":{"type":"string","description":"Only paths starting with this directory or prefix, for example internal/component/reward."},"glob":{"type":"string","description":"File-name glob such as *.go or *_test.go; a glob containing a slash is matched against the whole path."},"page":{"type":"integer","description":"1-based page of the result, default 1."},"limit":{"type":"integer","description":"Files per page, 1 to 1000, default 200."}},"additionalProperties":false}`),
			Handle:      t.listTree,
		},
		{
			Name:        "list_directory",
			Description: "List the immediate entries of a repository directory at the reviewed commit, with subdirectories marked by a trailing slash. Path is empty or omitted for the repository root.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Repository-relative directory path, empty for the root."}},"additionalProperties":false}`),
			Handle:      t.listDirectory,
		},
		{
			Name:        "list_pr_conversation",
			Description: "Read the pull request's review history in one view: every review with its author, state, submitted commit and summary, every review thread with its comments, is_resolved and is_outdated, and the conversation comments. Call this before judging, so a finding an earlier round already raised is never repeated and a resolved thread is read as a decision. Threads come newest first; when more were left out, next_before names the thread to pass as before.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"include_resolved":{"type":"boolean","description":"Include resolved threads. Default true."},"path":{"type":"string","description":"Only threads on this file path."},"before":{"type":"string","description":"A thread id from an earlier page; the result starts at the thread after it."},"limit":{"type":"integer","description":"Threads to return, 1 to 100, default 30."}},"additionalProperties":false}`),
			Handle:      t.listConversation,
		},
		{
			Name:        "resolve_review_thread",
			Description: "Close a review thread you opened, once the current code shows the finding is gone, and post the reason into the thread so the resolution is not silent. Only your own threads, only from the second review of a pull request, and at most 20 in one review.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"thread_id":{"type":"string","description":"The thread id from list_pr_conversation."},"reason":{"type":"string","description":"One line saying why the finding no longer applies."}},"required":["thread_id","reason"],"additionalProperties":false}`),
			Handle:      t.resolveThread,
		},
		{
			Name:        "submit_pr_review",
			Description: "Submit the review. Inline comments must target lines that exist in the diff on the requested side; comments outside the diff are moved into the summary instead of failing. Existing identical comments are skipped, and a rejected APPROVE or REQUEST_CHANGES is resubmitted as COMMENT.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"event":{"type":"string","enum":["COMMENT","REQUEST_CHANGES","APPROVE"]},"summary_body":{"type":"string"},"comments":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"line":{"type":"integer"},"side":{"type":"string","enum":["RIGHT","LEFT"]},"body":{"type":"string"}},"required":["path","line","body"],"additionalProperties":false}}},"required":["event","summary_body"],"additionalProperties":false}`),
			Handle:      t.submitReview,
		},
	}
	if t.snap != nil {
		specs = append(specs, Spec{
			Name:        "search_code",
			Description: "Search the repository at the reviewed commit with a regular expression and return path:line matches. Narrow it with path_prefix or glob; matches are capped, so refine the query rather than raising max_results blindly.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Go regular expression, for example CreateRewardHandler|func .*Reward.*Create."},"path_prefix":{"type":"string","description":"Only files under this directory or prefix."},"glob":{"type":"string","description":"File-name glob such as *.go; a glob containing a slash is matched against the whole path."},"max_results":{"type":"integer","description":"Matches to return, 1 to 200, default 50."},"ignore_case":{"type":"boolean","description":"Match case-insensitively."}},"required":["query"],"additionalProperties":false}`),
			Handle:      t.searchCode,
		})
	}
	return specs
}

func (t *GithubTools) getMetadata(ctx context.Context, _ map[string]any) (string, error) {
	meta, err := t.client.Metadata(ctx)
	if err != nil {
		return "", fmt.Errorf("get_pr_metadata failed: %w", err)
	}

	// The delta a re-review measures from. A failure to read the review list
	// leaves it empty, which sends the model back to the whole diff: the safe
	// direction, and not worth failing the metadata call over.
	lastReviewed := ""
	if reviews, err := t.readReviews(ctx); err != nil {
		t.logger.Warn("reading the review list for last_reviewed_commit failed", "error", err)
	} else {
		lastReviewed = ghapp.LastReviewedCommitOf(reviews, t.botLogin)
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
		"number":               meta.Number,
		"title":                meta.Title,
		"body":                 meta.Body,
		"author":               meta.Author,
		"state":                meta.State,
		"draft":                meta.Draft,
		"base_sha":             meta.BaseSHA,
		"head_sha":             meta.HeadSHA,
		"last_reviewed_commit": lastReviewed,
		"changed_files":        files,
	})
}

func (t *GithubTools) getDiff(ctx context.Context, args map[string]any) (string, error) {
	filePath, _ := args["file_path"].(string)
	since, _ := args["since"].(string)
	page := intArg(args, "page")

	diff, err := t.diff(ctx, since)
	if err != nil {
		return "", fmt.Errorf("get_pr_diff failed: %w", err)
	}

	header := ""
	if since != "" {
		header = fmt.Sprintf("diff of the commits after %s: only what the last review did not see\n", shortSHA(since))
		if len(diff.Paths()) == 0 {
			return header + "nothing changed after that commit\n", nil
		}
	}

	if filePath != "" {
		annotated := diff.Annotate(filePath)
		if annotated == "" {
			if since != "" {
				return header + fmt.Sprintf("%s has no changes after %s\n", filePath, shortSHA(since)), nil
			}
			return "", fmt.Errorf("%s is not part of this pull request diff", filePath)
		}
		return header + annotated, nil
	}

	pages := diff.AnnotatedPages(t.maxDiffBytes)
	if page < 1 {
		page = 1
	}
	if page > len(pages) {
		return "", fmt.Errorf("page %d does not exist, the diff has %d pages", page, len(pages))
	}

	if len(pages) > 1 {
		header += fmt.Sprintf("diff page %d of %d\ncall get_pr_diff again with the next page number for the rest of the diff\n", page, len(pages))
	}
	return header + pages[page-1], nil
}

// diff is the whole diff, or the part of it that landed after since.
func (t *GithubTools) diff(ctx context.Context, since string) (*ghapp.Diff, error) {
	if since == "" {
		return t.client.Diff(ctx)
	}
	meta, err := t.client.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the pull request metadata: %w", err)
	}
	return t.client.DiffSince(ctx, since, meta.HeadSHA)
}

func (t *GithubTools) getFileContents(ctx context.Context, args map[string]any) (string, error) {
	filePath, _ := args["file_path"].(string)
	ref, _ := args["ref"].(string)
	if filePath == "" {
		return "", fmt.Errorf("file_path is required")
	}

	if t.snap != nil && (ref == "" || ref == t.headSHA) {
		content, err := t.snap.Read(filePath, intArg(args, "start_line"), intArg(args, "end_line"))
		if err != nil {
			if errors.Is(err, snapshot.ErrNotFound) {
				return "", fmt.Errorf("%w (use list_repository_tree to find the exact path)", err)
			}
			return "", err
		}
		return content, nil
	}

	content, err := t.client.FileContents(ctx, filePath, ref, intArg(args, "start_line"), intArg(args, "end_line"))
	if err != nil {
		return "", err
	}
	return content, nil
}

// repositoryFiles is the tree of the reviewed commit, from the local snapshot
// when the session has one and from the GitHub tree API otherwise.
func (t *GithubTools) repositoryFiles(ctx context.Context) ([]snapshot.File, bool, error) {
	if t.snap != nil {
		return t.snap.All(), false, nil
	}
	sha := t.headSHA
	if sha == "" {
		meta, err := t.client.Metadata(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("reading the pull request metadata: %w", err)
		}
		sha = meta.HeadSHA
	}
	entries, cut, err := t.client.Tree(ctx, sha)
	if err != nil {
		return nil, false, err
	}
	files := make([]snapshot.File, 0, len(entries))
	for _, entry := range entries {
		files = append(files, snapshot.File{Path: entry.Path, Size: entry.Size})
	}
	return files, cut, nil
}

func (t *GithubTools) listTree(ctx context.Context, args map[string]any) (string, error) {
	prefix, _ := args["prefix"].(string)
	glob, _ := args["glob"].(string)
	limit := intArg(args, "limit")
	if limit < 1 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	page := intArg(args, "page")
	if page < 1 {
		page = 1
	}

	files, cut, err := t.repositoryFiles(ctx)
	if err != nil {
		return "", fmt.Errorf("list_repository_tree failed: %w", err)
	}
	selected, total, _ := snapshot.Filter(files, prefix, glob, (page-1)*limit, limit)

	var b strings.Builder
	if total == 0 {
		fmt.Fprintf(&b, "no files match prefix %q and glob %q at this commit\n", prefix, glob)
		return b.String(), nil
	}
	if (page-1)*limit >= total {
		return "", fmt.Errorf("page %d does not exist, %d files match", page, total)
	}
	for _, file := range selected {
		b.WriteString(file.Path)
		b.WriteByte('\n')
	}
	if shown := (page - 1) * limit; shown+len(selected) < total {
		fmt.Fprintf(&b, "\n[%d files match; this page shows %d-%d. Refine prefix or glob, or request page %d]\n",
			total, shown+1, shown+len(selected), page+1)
	}
	if cut {
		b.WriteString("\n[GitHub truncated the tree listing: refine prefix or glob]\n")
	}
	return b.String(), nil
}

func (t *GithubTools) listDirectory(ctx context.Context, args map[string]any) (string, error) {
	dir, _ := args["path"].(string)
	files, _, err := t.repositoryFiles(ctx)
	if err != nil {
		return "", fmt.Errorf("list_directory failed: %w", err)
	}
	entries, err := snapshot.EntriesOf(files, dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "the directory is empty at this commit\n", nil
	}

	var b strings.Builder
	for _, entry := range entries {
		if entry.Dir {
			b.WriteString(entry.Path)
			b.WriteByte('/')
			b.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&b, "%s (%s)\n", entry.Path, snapshot.FormatSize(entry.Size))
	}
	return b.String(), nil
}

func (t *GithubTools) searchCode(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	prefix, _ := args["path_prefix"].(string)
	glob, _ := args["glob"].(string)
	ignoreCase, _ := args["ignore_case"].(bool)
	maxResults := intArg(args, "max_results")
	if maxResults < 1 {
		maxResults = 50
	}
	if maxResults > 200 {
		maxResults = 200
	}
	if t.snap == nil {
		return "", fmt.Errorf("search_code needs the local repository snapshot, which this session does not have")
	}

	matches, more, err := t.snap.Search(ctx, query, prefix, glob, maxResults, ignoreCase)
	if err != nil {
		return "", fmt.Errorf("search_code failed: %w", err)
	}
	if len(matches) == 0 {
		return "no matches\n", nil
	}

	var b strings.Builder
	for _, match := range matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", match.Path, match.Line, match.Text)
	}
	if more {
		fmt.Fprintf(&b, "\n[stopped after %d matches; narrow the query with path_prefix or glob]\n", len(matches))
	}
	return b.String(), nil
}

// shortSHA is the seven-character form of a commit, which is what fits in a
// header and what people write.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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

// intArg reads an integer argument, accepting the JSON number types a decoded
// argument map may carry.
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
