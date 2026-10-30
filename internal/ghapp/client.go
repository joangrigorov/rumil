package ghapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v69/github"
)

// MaxFileReadLines caps a single get_file_contents read.
const MaxFileReadLines = 500

// MaxGuidelinesBytes caps the repository review guidelines injected into the
// system prompt, so a generated file cannot overflow the model's context.
const MaxGuidelinesBytes = 32 << 10

// GuidelinesPath is the repository review guidelines file (§6 Phase 2), read
// from the repository root. The name is Rumil's own so that a REVIEW.md kept
// for another purpose is never picked up.
const GuidelinesPath = "RUMIL.md"

// maxFileBytes bounds how much of a single file is downloaded.
const maxFileBytes = 8 << 20

// contentEncodingBase64 is what the contents API reports for an inlined file.
const contentEncodingBase64 = "base64"

// Review events accepted by submit_pr_review.
const (
	EventComment        = "COMMENT"
	EventRequestChanges = "REQUEST_CHANGES"
	EventApprove        = "APPROVE"
)

// Client is the read-only GitHub surface for one pull request. It never
// mutates the repository: its writes are the review, the thread reasons it
// posts, and the check run that reports the review's progress.
type Client struct {
	gh     *github.Client
	owner  string
	repo   string
	number int

	mu   sync.Mutex
	meta *PRMetadata
	diff *Diff

	treeMu   sync.Mutex
	treeSHA  string
	tree     []TreeEntry
	treeCut  bool
	treeRead bool
	treeErr  error
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
	// InReplyToID is the comment this one replies to, and zero when it opened
	// the thread. It is what makes a thread walkable.
	InReplyToID int64
	// CreatedAt orders the comments of a thread. Two comments can share a
	// second, so the id is the tie-breaker.
	CreatedAt time.Time
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

// SubmittedFinding is one inline comment GitHub accepted, with the id later
// feedback arrives on.
type SubmittedFinding struct {
	CommentID int64
	Path      string
	Line      int
	Body      string
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
	// ReviewID is the review GitHub created, which later dismissal events name.
	ReviewID int64
	// Findings are the inline comments GitHub accepted, with their comment ids.
	Findings []SubmittedFinding
	// FindingsError reports a review that submitted but whose comment ids could
	// not be read back. The review is valid; only outcome tracking is missing.
	FindingsError string
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

// TreeEntry is one file of the repository tree at the reviewed commit.
type TreeEntry struct {
	Path string
	Size int64
}

// Tree returns every file path in the repository at sha, read once per client.
// cut reports that GitHub truncated the listing, which only happens on
// repositories far larger than a review can read anyway.
func (c *Client) Tree(ctx context.Context, sha string) (entries []TreeEntry, cut bool, err error) {
	c.treeMu.Lock()
	if c.treeRead && c.treeSHA == sha {
		files, truncated, readErr := c.tree, c.treeCut, c.treeErr
		c.treeMu.Unlock()
		return append([]TreeEntry(nil), files...), truncated, readErr
	}
	c.treeMu.Unlock()

	tree, _, err := c.gh.Git.GetTree(ctx, c.owner, c.repo, sha, true)
	if err != nil {
		err = fmt.Errorf("reading the repository tree at %s: %w", shortSHA(sha), err)
	} else {
		for _, entry := range tree.Entries {
			if entry.GetType() != "blob" {
				continue
			}
			entries = append(entries, TreeEntry{Path: entry.GetPath(), Size: int64(entry.GetSize())})
		}
	}

	c.treeMu.Lock()
	c.treeSHA = sha
	c.tree = entries
	c.treeCut = err == nil && tree != nil && tree.GetTruncated()
	c.treeRead = true
	c.treeErr = err
	cut = c.treeCut
	c.treeMu.Unlock()

	return append([]TreeEntry(nil), entries...), cut, err
}

// Archive returns the repository tarball at sha for the session's working
// tree. The caller owns the reader and must close it.
//
// GitHub answers the API endpoint with a short-lived signed URL, so the
// download goes through a plain client: the installation token must not travel
// to the archive host. Errors are returned without that URL, which carries the
// signature.
func (c *Client) Archive(ctx context.Context, sha string) (io.ReadCloser, error) {
	link, _, err := c.gh.Repositories.GetArchiveLink(ctx, c.owner, c.repo, github.Tarball,
		&github.RepositoryContentGetOptions{Ref: sha}, 0)
	if err != nil {
		return nil, fmt.Errorf("asking GitHub for the repository archive at %s: %w", shortSHA(sha), err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building the archive request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("downloading the repository archive at %s: %w", shortSHA(sha), err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("downloading the repository archive at %s: %s", shortSHA(sha), resp.Status)
	}
	return resp.Body, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (c *Client) diffLocked(ctx context.Context) (*Diff, error) {
	if c.diff != nil {
		return c.diff, nil
	}

	raw, _, err := c.gh.PullRequests.GetRaw(ctx, c.owner, c.repo, c.number, github.RawOptions{Type: github.Diff})
	if err != nil {
		if !diffTooLarge(err) {
			return nil, fmt.Errorf("fetching diff for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		// GitHub refuses a diff over 300 files with 406. The files endpoint
		// carries the same per-file patches one page at a time, so the review
		// can still read the change and still place its inline comments.
		parsed, fallbackErr := c.filesDiffLocked(ctx)
		if fallbackErr != nil {
			return nil, fmt.Errorf("fetching diff for %s/%s#%d: %w (the files API fallback failed: %v)",
				c.owner, c.repo, c.number, err, fallbackErr)
		}
		c.diff = parsed
		return c.diff, nil
	}

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing diff for %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}
	c.diff = parsed
	return c.diff, nil
}

// diffTooLarge reports the 406 GitHub answers for a pull request over its
// 300-file diff limit.
func diffTooLarge(err error) bool {
	var response *github.ErrorResponse
	return errors.As(err, &response) && response.Response != nil &&
		response.Response.StatusCode == http.StatusNotAcceptable
}

// filesDiffLocked builds the diff from the pull request files endpoint. Each
// entry carries the hunks without the file headers, so they are synthesized in
// the format ParseUnifiedDiff reads. GitHub omits the patch of a binary file
// and of a file whose diff is too large to inline; such a file stays named in
// the diff with no lines, and a comment on it moves into the summary.
func (c *Client) filesDiffLocked(ctx context.Context) (*Diff, error) {
	var raw strings.Builder
	opts := &github.ListOptions{PerPage: 100}
	for {
		files, response, err := c.gh.PullRequests.ListFiles(ctx, c.owner, c.repo, c.number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing files of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, file := range files {
			writeFileDiff(&raw, file)
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}

	parsed, err := ParseUnifiedDiff(raw.String())
	if err != nil {
		return nil, fmt.Errorf("parsing the files of %s/%s#%d: %w", c.owner, c.repo, c.number, err)
	}
	return parsed, nil
}

// writeFileDiff writes one files-endpoint entry as a unified diff section.
func writeFileDiff(b *strings.Builder, file *github.CommitFile) {
	path := file.GetFilename()
	oldPath := file.GetPreviousFilename()
	if oldPath == "" {
		oldPath = path
	}

	oldHeader, newHeader := "a/"+oldPath, "b/"+path
	switch file.GetStatus() {
	case "added":
		oldHeader = "/dev/null"
	case "removed":
		newHeader = "/dev/null"
	}
	fmt.Fprintf(b, "diff --git a/%s b/%s\n--- %s\n+++ %s\n", oldPath, path, oldHeader, newHeader)

	if patch := file.GetPatch(); patch != "" {
		b.WriteString(patch)
		if !strings.HasSuffix(patch, "\n") {
			b.WriteString("\n")
		}
		return
	}
	if file.GetStatus() != "renamed" && file.GetAdditions() == 0 && file.GetDeletions() == 0 {
		fmt.Fprintf(b, "Binary files a/%s and b/%s differ\n", oldPath, path)
	}
}

// ReviewGuidelines fetches the repository's review guidelines, GuidelinesPath
// at the repository root and nothing else, and returns the path with the text.
// A repository without the file returns empty strings and a nil error, and the
// review proceeds on the default prompt.
//
// The text is capped at MaxGuidelinesBytes with a truncation marker. Callers
// pass the pull request base SHA so an untrusted branch cannot rewrite the
// repository's review policy.
func (c *Client) ReviewGuidelines(ctx context.Context, ref string) (string, string, error) {
	if ref == "" {
		meta, err := c.Metadata(ctx)
		if err != nil {
			return "", "", err
		}
		ref = meta.BaseSHA
	}

	file, err := c.fileAt(ctx, GuidelinesPath, ref)
	if err != nil {
		return "", "", err
	}
	if file == nil {
		return "", "", nil
	}

	text, err := c.fileText(ctx, file, GuidelinesPath, ref)
	if err != nil {
		return "", "", err
	}
	return GuidelinesPath, truncateGuidelines(text), nil
}

// RepositoryFile reads one repository file at ref, capped at maxBytes. A path
// that holds no file at that ref is not an error: it returns ok false, which
// is how an optional settings file is read.
func (c *Client) RepositoryFile(ctx context.Context, path, ref string, maxBytes int) (string, bool, error) {
	if ref == "" {
		meta, err := c.Metadata(ctx)
		if err != nil {
			return "", false, err
		}
		ref = meta.BaseSHA
	}

	file, err := c.fileAt(ctx, path, ref)
	if err != nil {
		return "", false, err
	}
	if file == nil {
		return "", false, nil
	}
	if size := file.GetSize(); maxBytes > 0 && size > maxBytes {
		return "", false, fmt.Errorf("%s at %s is %d bytes, larger than the %d byte limit", path, ref, size, maxBytes)
	}

	text, err := c.fileText(ctx, file, path, ref)
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(text) > maxBytes {
		return "", false, fmt.Errorf("%s at %s is %d bytes, larger than the %d byte limit", path, ref, len(text), maxBytes)
	}
	return text, true, nil
}

// truncateGuidelines caps the guidelines at MaxGuidelinesBytes, cutting on a
// rune boundary and appending a marker that names the original size.
func truncateGuidelines(text string) string {
	if len(text) <= MaxGuidelinesBytes {
		return text
	}
	cut := text[:MaxGuidelinesBytes]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%s\n\n[truncated at %d bytes: the guidelines file is %d bytes]", cut, MaxGuidelinesBytes, len(text))
}

// fileText decodes the content of one contents-API entry, reading the blob it
// points at when the API declines to inline a large file.
func (c *Client) fileText(ctx context.Context, file *github.RepositoryContent, path, ref string) (string, error) {
	if file.GetEncoding() != contentEncodingBase64 {
		// The contents API inlines files up to 1 MB and reports encoding "none"
		// above that, so read the blob the entry points at.
		blob, _, err := c.gh.Git.GetBlob(ctx, c.owner, c.repo, file.GetSHA())
		if err != nil {
			return "", fmt.Errorf("reading the blob for %s at %s: %w", path, ref, err)
		}
		return decodeBlob(blob)
	}

	decoded, err := file.GetContent()
	if err != nil {
		return "", fmt.Errorf("decoding %s at %s: %w", path, ref, err)
	}
	return decoded, nil
}

// decodeBlob returns the text of a blob API entry, which arrives base64
// encoded and wrapped across lines.
func decodeBlob(blob *github.Blob) (string, error) {
	if encoding := blob.GetEncoding(); encoding != contentEncodingBase64 {
		return "", fmt.Errorf("blob %s is encoded as %q, want base64", blob.GetSHA(), encoding)
	}
	compact := strings.NewReplacer("\n", "", "\r", "").Replace(blob.GetContent())
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return "", fmt.Errorf("decoding blob %s: %w", blob.GetSHA(), err)
	}
	return string(decoded), nil
}

// fileAt returns the contents-API entry for path at ref, or nil when the path
// holds no file at that ref. A missing path is not an error, so callers can
// walk a fallback list.
func (c *Client) fileAt(ctx context.Context, path, ref string) (*github.RepositoryContent, error) {
	file, _, resp, err := c.gh.Repositories.GetContents(ctx, c.owner, c.repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s at %s in %s/%s: %w", path, ref, c.owner, c.repo, err)
	}
	return file, nil
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
				ID:          cm.GetID(),
				Path:        cm.GetPath(),
				Line:        cm.GetLine(),
				Body:        cm.GetBody(),
				InReplyToID: cm.GetInReplyTo(),
				CreatedAt:   cm.GetCreatedAt().Time,
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
	outcome.ReviewID = created.GetID()

	// The review is already on the pull request, so a failure to read back the
	// comment ids is reported, not returned: the review succeeded.
	findings, findingsErr := c.reviewFindings(ctx, outcome.ReviewID, len(inline))
	outcome.Findings = findings
	if findingsErr != nil {
		outcome.FindingsError = findingsErr.Error()
	}
	return outcome, nil
}

// maxFindingPages bounds the read-back that maps a just-created review onto its
// comment ids. A review with more comments than this is recorded in part.
const maxFindingPages = 10

// reviewFindings lists the comments the review just created, newest first, and
// stops once it has the expected number. The list endpoint filters by pull
// request rather than by review, so the review id is matched here.
func (c *Client) reviewFindings(ctx context.Context, reviewID int64, expected int) ([]SubmittedFinding, error) {
	if reviewID == 0 || expected == 0 {
		return nil, nil
	}

	findings := make([]SubmittedFinding, 0, expected)
	options := &github.PullRequestListCommentsOptions{
		Sort:        "created",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for page := 0; page < maxFindingPages && len(findings) < expected; page++ {
		comments, response, err := c.gh.PullRequests.ListComments(ctx, c.owner, c.repo, c.number, options)
		if err != nil {
			return findings, fmt.Errorf("listing the review's comments on %s/%s#%d: %w", c.owner, c.repo, c.number, err)
		}
		for _, comment := range comments {
			if comment.GetPullRequestReviewID() != reviewID {
				continue
			}
			findings = append(findings, SubmittedFinding{
				CommentID: comment.GetID(),
				Path:      comment.GetPath(),
				Line:      comment.GetLine(),
				Body:      comment.GetBody(),
			})
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	return findings, nil
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

// ReviewCheckName is the check run that reports one review's progress. It is a
// single entry per commit: a continuation or a webhook redelivery updates it
// rather than adding another.
const ReviewCheckName = "Rumil review"

// maxCheckSummaryBytes caps the one-line summary a check run carries, so a
// provider error cannot fill the check page.
const maxCheckSummaryBytes = 500

// StartReviewCheck opens the check run for the commit under review, reusing the
// latest one with the same name. A check run is the only progress a reviewer
// sees where they already look, and it avoids a progress comment per turn.
func (c *Client) StartReviewCheck(ctx context.Context, headSHA, detailsURL string, appID int64) (int64, error) {
	if headSHA == "" {
		return 0, errors.New("a check run needs the commit under review")
	}

	output := &github.CheckRunOutput{
		Title:   github.Ptr(ReviewCheckName),
		Summary: github.Ptr("Reviewing the pull request."),
	}
	existing, err := c.latestReviewCheck(ctx, headSHA, appID)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		id := existing.GetID()
		update := github.UpdateCheckRunOptions{
			Name:   ReviewCheckName,
			Status: github.Ptr("in_progress"),
			Output: output,
		}
		if detailsURL != "" {
			update.DetailsURL = github.Ptr(detailsURL)
		}
		if _, _, err := c.gh.Checks.UpdateCheckRun(ctx, c.owner, c.repo, id, update); err != nil {
			return 0, fmt.Errorf("restarting check run %d for %s/%s@%s: %w", id, c.owner, c.repo, headSHA, err)
		}
		return id, nil
	}

	create := github.CreateCheckRunOptions{
		Name:    ReviewCheckName,
		HeadSHA: headSHA,
		Status:  github.Ptr("in_progress"),
		Output:  output,
	}
	if detailsURL != "" {
		create.DetailsURL = github.Ptr(detailsURL)
	}
	run, _, err := c.gh.Checks.CreateCheckRun(ctx, c.owner, c.repo, create)
	if err != nil {
		return 0, fmt.Errorf("creating a check run for %s/%s@%s: %w", c.owner, c.repo, headSHA, err)
	}
	return run.GetID(), nil
}

// FinishReviewCheck completes a check run with a conclusion and one summary
// line. A zero id is a session that never opened one, which is not an error.
func (c *Client) FinishReviewCheck(ctx context.Context, id int64, conclusion, summary string) error {
	if id == 0 {
		return nil
	}
	if conclusion == "" {
		return errors.New("a completed check run needs a conclusion")
	}
	_, _, err := c.gh.Checks.UpdateCheckRun(ctx, c.owner, c.repo, id, github.UpdateCheckRunOptions{
		Name:        ReviewCheckName,
		Status:      github.Ptr("completed"),
		Conclusion:  github.Ptr(conclusion),
		CompletedAt: &github.Timestamp{Time: time.Now()},
		Output: &github.CheckRunOutput{
			Title:   github.Ptr(ReviewCheckName),
			Summary: github.Ptr(truncateCheckSummary(summary)),
		},
	})
	if err != nil {
		return fmt.Errorf("completing check run %d for %s/%s#%d: %w", id, c.owner, c.repo, c.number, err)
	}
	return nil
}

// latestReviewCheck is the newest check run with the review's name that this
// App opened on the commit, or nil when it has not opened one. The app id is
// part of the filter because another App may use the same check name.
func (c *Client) latestReviewCheck(ctx context.Context, headSHA string, appID int64) (*github.CheckRun, error) {
	options := &github.ListCheckRunsOptions{
		CheckName: github.Ptr(ReviewCheckName),
		Filter:    github.Ptr("latest"),
	}
	if appID > 0 {
		options.AppID = github.Ptr(appID)
	}
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, headSHA, options)
	if err != nil {
		return nil, fmt.Errorf("listing check runs for %s/%s@%s: %w", c.owner, c.repo, headSHA, err)
	}
	if runs == nil || len(runs.CheckRuns) == 0 {
		return nil, nil
	}
	return runs.CheckRuns[0], nil
}

// truncateCheckSummary caps a summary at a rune boundary.
func truncateCheckSummary(summary string) string {
	if len(summary) <= maxCheckSummaryBytes {
		return summary
	}
	cut := maxCheckSummaryBytes
	for cut > 0 && !utf8.RuneStart(summary[cut]) {
		cut--
	}
	return strings.TrimSpace(summary[:cut])
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
