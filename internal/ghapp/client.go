package ghapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-github/v69/github"
)

// MaxFileReadLines caps a single get_file_contents read.
const MaxFileReadLines = 500

// maxFileBytes bounds how much of a single file is downloaded.
const maxFileBytes = 8 << 20

// Review events accepted by submit_pr_review.
const (
	EventComment        = "COMMENT"
	EventRequestChanges = "REQUEST_CHANGES"
	EventApprove        = "APPROVE"
)

// Client is the read-only GitHub surface for one pull request. It never
// mutates the repository: the only write it performs is submitting a review.
type Client struct {
	gh     *github.Client
	owner  string
	repo   string
	number int

	mu   sync.Mutex
	meta *PRMetadata
	diff *Diff
}

// NewClient builds a client scoped to a single pull request.
func NewClient(gh *github.Client, owner, repo string, number int) *Client {
	return &Client{gh: gh, owner: owner, repo: repo, number: number}
}

// ChangedFile is one entry of the pull request file list.
type ChangedFile struct {
	Path      string
	Additions int
	Deletions int
}

// PRMetadata is the pull request header plus its changed files.
type PRMetadata struct {
	Number       int
	Title        string
	Body         string
	Author       string
	State        string
	Draft        bool
	BaseSHA      string
	HeadSHA      string
	ChangedFiles []ChangedFile
}

// ExistingComment is a review comment already present on the pull request.
type ExistingComment struct {
	ID     int64
	Path   string
	Line   int
	Body   string
	Author string
}

// ReviewComment is one inline comment the reviewer wants to post.
type ReviewComment struct {
	Path string
	Line int
	Side string
	Body string
}

// ReviewRequest is a batch review submission.
type ReviewRequest struct {
	Event       string
	SummaryBody string
	Comments    []ReviewComment
}

// ReviewOutcome reports what GitHub actually accepted.
type ReviewOutcome struct {
	// Event is the event GitHub recorded.
	Event string
	// RequestedEvent is the event the reviewer asked for.
	RequestedEvent string
	// Fallback is true when a rejected stronger event was resubmitted as COMMENT.
	Fallback bool
	// MovedToSummary holds comments that targeted lines outside the diff,
	// rendered as "[path:line] body".
	MovedToSummary []string
	// SkippedDuplicates holds comments that already exist on the pull request,
	// rendered as "[path:line] body".
	SkippedDuplicates []string
}

// Metadata returns the pull request header and changed files, fetching them
// once per client.
func (c *Client) Metadata(ctx context.Context) (*PRMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.meta != nil {
		return c.meta, nil
	}

	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, c.number)
	if err != nil {
		return nil, fmt.Errorf("fetching pull request %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}

	meta := &PRMetadata{
		Number:  c.number,
		Title:   pr.GetTitle(),
		Body:    pr.GetBody(),
		State:   pr.GetState(),
		Draft:   pr.GetDraft(),
		BaseSHA: pr.GetBase().GetSHA(),
		HeadSHA: pr.GetHead().GetSHA(),
	}
	if user := pr.GetUser(); user != nil {
		meta.Author = user.GetLogin()
	}

	opts := &github.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.gh.PullRequests.ListFiles(ctx, c.owner, c.repo, c.number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing files of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, f := range files {
			meta.ChangedFiles = append(meta.ChangedFiles, ChangedFile{
				Path:      f.GetFilename(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	c.meta = meta
	return c.meta, nil
}

// Diff returns the parsed unified diff, fetching it once per client.
func (c *Client) Diff(ctx context.Context) (*Diff, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diffLocked(ctx)
}

func (c *Client) diffLocked(ctx context.Context) (*Diff, error) {
	if c.diff != nil {
		return c.diff, nil
	}

	raw, _, err := c.gh.PullRequests.GetRaw(ctx, c.owner, c.repo, c.number, github.RawOptions{Type: github.Diff})
	if err != nil {
		return nil, fmt.Errorf("fetching diff for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing diff for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}
	c.diff = parsed
	return c.diff, nil
}

// FileContents reads a line range of a repository file at ref, defaulting to
// the pull request head SHA. Binary files are rejected and reads are capped at
// MaxFileReadLines lines.
func (c *Client) FileContents(ctx context.Context, path, ref string, startLine, endLine int) (string, error) {
	if path == "" {
		return "", errors.New("file_path is required")
	}
	if ref == "" {
		meta, err := c.Metadata(ctx)
		if err != nil {
			return "", err
		}
		ref = meta.HeadSHA
	}

	file, _, _, err := c.gh.Repositories.GetContents(ctx, c.owner, c.repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", fmt.Errorf("reading %s at %s in %s/%s: %w", path, ref, c.owner, c.repo, err)
	}
	if file == nil {
		return "", fmt.Errorf("%s is not a file", path)
	}
	if size := file.GetSize(); size > maxFileBytes {
		return "", fmt.Errorf("%s at %s is %d bytes, larger than the %d byte read limit", path, ref, size, maxFileBytes)
	}
	// GetContent decodes the base64 payload that the contents API returns.
	decoded, err := file.GetContent()
	if err != nil {
		return "", fmt.Errorf("decoding %s at %s: %w", path, ref, err)
	}
	if decoded == "" && file.GetSize() > 0 {
		return "", fmt.Errorf("%s at %s is not returned inline by the contents API (%d bytes)", path, ref, file.GetSize())
	}

	raw := []byte(decoded)
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file and cannot be read", path)
	}

	lines := splitLines(decoded)
	if startLine < 1 {
		startLine = 1
	}
	if startLine > len(lines) {
		return "", fmt.Errorf("%s has %d lines, start_line %d is past the end", path, len(lines), startLine)
	}
	if endLine < 1 || endLine > len(lines) {
		endLine = len(lines)
	}

	selected := lines[startLine-1 : endLine]
	truncated := false
	if len(selected) > MaxFileReadLines {
		selected = selected[:MaxFileReadLines]
		truncated = true
	}

	out := strings.Join(selected, "\n")
	if truncated {
		out += fmt.Sprintf("\n[truncated at %d lines: requested %d-%d of a %d line file]",
			MaxFileReadLines, startLine, endLine, len(lines))
	}
	return out, nil
}

// ExistingReviewComments returns the review comments already posted on the
// pull request so findings are never repeated.
func (c *Client) ExistingReviewComments(ctx context.Context) ([]ExistingComment, error) {
	var comments []ExistingComment
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		page, resp, err := c.gh.PullRequests.ListComments(ctx, c.owner, c.repo, c.number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing review comments of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, cm := range page {
			comment := ExistingComment{
				ID:   cm.GetID(),
				Path: cm.GetPath(),
				Line: cm.GetLine(),
				Body: cm.GetBody(),
			}
			if user := cm.GetUser(); user != nil {
				comment.Author = user.GetLogin()
			}
			comments = append(comments, comment)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return comments, nil
}

// SubmitReview validates every inline comment against the diff, keeps the
// valid ones inline, folds the rest into the summary body, drops comments that
// already exist on the pull request, and falls back to COMMENT when GitHub
// rejects a stronger event.
func (c *Client) SubmitReview(ctx context.Context, diff *Diff, req ReviewRequest) (*ReviewOutcome, error) {
	existing, err := c.ExistingReviewComments(ctx)
	if err != nil {
		return nil, err
	}

	outcome := &ReviewOutcome{Event: req.Event, RequestedEvent: req.Event}
	summary := strings.TrimRight(req.SummaryBody, "\n")
	inline := make([]*github.DraftReviewComment, 0, len(req.Comments))

	for _, comment := range req.Comments {
		if !diff.HasNewLine(comment.Path, comment.Line) {
			outcome.MovedToSummary = append(outcome.MovedToSummary,
				fmt.Sprintf("[%s:%d] %s", comment.Path, comment.Line, comment.Body))
			continue
		}
		if isDuplicate(existing, comment) {
			outcome.SkippedDuplicates = append(outcome.SkippedDuplicates,
				fmt.Sprintf("[%s:%d] %s", comment.Path, comment.Line, comment.Body))
			continue
		}
		side := comment.Side
		if side == "" {
			side = "RIGHT"
		}
		inline = append(inline, &github.DraftReviewComment{
			Path: github.Ptr(comment.Path),
			Line: github.Ptr(comment.Line),
			Side: github.Ptr(side),
			Body: github.Ptr(comment.Body),
		})
	}

	if len(outcome.MovedToSummary) > 0 {
		summary = joinSections(summary, "Findings outside the diff:\n"+strings.Join(outcome.MovedToSummary, "\n"))
	}

	review := &github.PullRequestReviewRequest{
		Event:    github.Ptr(req.Event),
		Body:     github.Ptr(summary),
		Comments: inline,
	}

	created, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, c.number, review)
	if err != nil {
		if !canFallbackToComment(req.Event) || !isClientError(err) {
			return nil, fmt.Errorf("submitting review for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		review.Event = github.Ptr(EventComment)
		review.Body = github.Ptr(joinSections(
			fmt.Sprintf("Review event %s was rejected by GitHub, submitting as %s instead.", req.Event, EventComment),
			summary))
		created, _, err = c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, c.number, review)
		if err != nil {
			return nil, fmt.Errorf("submitting fallback review for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		outcome.Fallback = true
		outcome.Event = EventComment
	}

	if created != nil && reviewStateToEvent(created.GetState()) != "" {
		outcome.Event = reviewStateToEvent(created.GetState())
	}
	return outcome, nil
}

// reviewStateToEvent maps the state of a created review back onto the event
// vocabulary used by submit_pr_review.
func reviewStateToEvent(state string) string {
	switch state {
	case "APPROVED":
		return EventApprove
	case "CHANGES_REQUESTED":
		return EventRequestChanges
	case "COMMENTED":
		return EventComment
	default:
		return ""
	}
}

func isDuplicate(existing []ExistingComment, comment ReviewComment) bool {
	for _, e := range existing {
		if e.Path == comment.Path && e.Line == comment.Line && strings.TrimSpace(e.Body) == strings.TrimSpace(comment.Body) {
			return true
		}
	}
	return false
}

func canFallbackToComment(event string) bool {
	return event == EventApprove || event == EventRequestChanges
}

func isClientError(err error) bool {
	var errResp *github.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		status := errResp.Response.StatusCode
		return status >= http.StatusBadRequest && status < http.StatusInternalServerError
	}
	return false
}

func joinSections(sections ...string) string {
	var kept []string
	for _, s := range sections {
		if strings.TrimSpace(s) != "" {
			kept = append(kept, strings.TrimRight(s, "\n"))
		}
	}
	return strings.Join(kept, "\n\n")
}
