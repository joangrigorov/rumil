// Package testutil provides the hermetic doubles the verification matrix
// needs: a fake GitHub REST API, a fake OpenAI-compatible chat endpoint, and
// test key material. Nothing here talks to the network outside httptest.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// FileEntry is one entry in the fake pull request file list.
type FileEntry struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Status           string `json:"status"`
	// Patch is the hunk text the files endpoint carries for a changed file.
	Patch string `json:"patch,omitempty"`
}

// CommentEntry is one existing review comment.
type CommentEntry struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	// InReplyToID is zero for a comment that opened a thread.
	InReplyToID int64 `json:"in_reply_to_id"`
	// PullRequestReviewID is the review a submitted inline comment belongs to,
	// which is how the client maps a created review back onto its comments.
	PullRequestReviewID int64 `json:"pull_request_review_id,omitempty"`
	// CreatedAt is when the comment was written, which is how threads are
	// ordered newest first.
	CreatedAt time.Time `json:"created_at"`
}

// PostedComment is one comment Rumil posted: a reply inside a review thread, or
// a conversation comment when InReplyTo is zero.
type PostedComment struct {
	Body      string `json:"body"`
	InReplyTo int64  `json:"in_reply_to,omitempty"`
}

// ReviewComment builds a comment fixture for the fake's comment endpoints. The
// reported type follows the login, so a bot fixture looks like one, and the
// timestamp follows the id, so a higher id is a newer comment.
func ReviewComment(id, inReplyTo int64, login, body string) CommentEntry {
	entry := CommentEntry{
		ID:          id,
		Path:        "main.go",
		Body:        body,
		InReplyToID: inReplyTo,
		CreatedAt:   time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Minute),
	}
	entry.User.Login = login
	entry.User.Type = "User"
	if strings.HasSuffix(login, "[bot]") {
		entry.User.Type = "Bot"
	}
	return entry
}

// ReviewEntry is one review the pull request already received.
type ReviewEntry struct {
	ID       int64  `json:"id"`
	Author   string `json:"author"`
	State    string `json:"state"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
}

// ThreadEntry is one review thread the GraphQL endpoint serves.
type ThreadEntry struct {
	ID         string         `json:"id"`
	Path       string         `json:"path"`
	Line       int            `json:"line"`
	IsResolved bool           `json:"is_resolved"`
	IsOutdated bool           `json:"is_outdated"`
	Comments   []CommentEntry `json:"comments"`
}

// SubmittedReview captures a POST to the reviews endpoint.
type SubmittedReview struct {
	Event    string `json:"event"`
	Body     string `json:"body"`
	Comments []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	} `json:"comments"`
}

// Reaction is one acknowledgement, as the API received it.
type Reaction struct {
	// Target is "pull_request", "issue_comment" or "review_comment".
	Target string
	// ID is the comment the reaction hangs on, zero for the pull request body.
	ID      int64
	Content string
}

// CheckRunEntry is one check run the fake received: the state a reviewer would
// see in the pull request's checks tab.
type CheckRunEntry struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
	Summary    string `json:"summary"`
}

// FakeGitHub is an httptest-backed stand-in for the GitHub REST API surface
// that Rumil uses.
type FakeGitHub struct {
	Server *httptest.Server

	Owner  string
	Repo   string
	Number int

	PRTitle  string
	PRBody   string
	PRAuthor string
	PRState  string
	BaseSHA  string
	HeadSHA  string

	Diff string
	// DiffStatus, when non-zero, is returned by the diff endpoint instead of
	// the diff, so a test can drive the files-API fallback.
	DiffStatus int
	Files      []FileEntry
	Contents   map[string]string
	// ContentsByRef overrides Contents for a specific ref, so a test can serve
	// different text on the base SHA and the head SHA.
	ContentsByRef map[string]map[string]string
	// Tarball is the repository archive the fake serves for the reviewed
	// commit, as a gzipped tar with a single top directory. NewFakeGitHub
	// builds it from Contents; a test that wants a different tree sets it.
	Tarball []byte
	// TarballStatus, when non-zero, is returned by the archive endpoint
	// instead of a redirect, so a test can drive the snapshot fallback.
	TarballStatus int
	// Blobs backs the git blobs endpoint. handleContents fills it for files the
	// contents API declines to inline.
	Blobs map[string]string
	// Comments are the review comments the pull request already carries.
	Comments []CommentEntry
	// IssueComments are the conversation comments the pull request already
	// carries.
	IssueComments []CommentEntry
	// ExistingReviews are the reviews the pull request received before this
	// session, oldest first. Reviews submitted through the fake are appended to
	// them when the list is served.
	ExistingReviews []ReviewEntry
	// Threads are the review threads the GraphQL endpoint serves, and
	// ResolvedThreads records the ids the mutation was called with.
	Threads         []ThreadEntry
	ResolvedThreads []string
	// SinceDiff is served by the compare endpoint, which is how a later round
	// reads only the new commits.
	SinceDiff string
	// GraphQLErrors, when set, is returned in the errors array of every
	// GraphQL response.
	GraphQLErrors []string
	// ResolveError, when set, is returned by the mutation that closes a review
	// thread instead of it, so a test can drive a refusal of that operation
	// alone.
	ResolveError string
	// ThreadPageSize, when set, is how many threads one reviewThreads page
	// holds, so a test can make the client follow endCursor.
	ThreadPageSize int
	// Posted collects the comments Rumil posted through the reply and
	// conversation endpoints.
	Posted []PostedComment
	// Reactions collects the reactions Rumil added, in order, so a test can
	// assert what a request was acknowledged with and where.
	Reactions []Reaction
	// ReactionStatus, when non-zero, is returned by the reactions endpoints
	// instead of a created reaction.
	ReactionStatus int
	// BotLogin is the login the fake reports for the comments it records a post
	// as, so a later listing sees them exactly as GitHub would serve them.
	BotLogin string
	// AppSlug is the slug the fake's own App record carries, which is what the
	// configured bot handle is held against.
	AppSlug string
	// AppPermissions is the permission set the fake's App record carries, which
	// is what tells an operator that a capability will be refused before a run
	// spends anything on it.
	AppPermissions map[string]string
	// AppStatus, when non-zero, is returned by the App record instead of it.
	AppStatus int

	// MintStatus, when non-zero, is returned by the installation token
	// endpoint instead of a token (404 simulates an App that is not installed).
	MintStatus int
	// ReviewStatus, when non-zero, is returned by the first review POST.
	ReviewStatus int
	// CheckRuns are the check runs the fake has opened or updated, oldest
	// first.
	CheckRuns []CheckRunEntry
	// CheckStatus, when non-zero, is returned by the check-run endpoints, so a
	// test can drive an App without the Checks permission.
	CheckStatus int

	mu             sync.Mutex
	Mints          int
	Reviews        []SubmittedReview
	Requests       []string
	reviewFailures int
}

// NewFakeGitHub starts the fake API with a single changed file.
func NewFakeGitHub(t *testing.T) *FakeGitHub {
	t.Helper()

	f := &FakeGitHub{
		Owner:          "acme",
		Repo:           "payments",
		Number:         42,
		AppSlug:        "rumil",
		AppPermissions: map[string]string{"contents": "write", "pull_requests": "write"},
		PRTitle:        "Add retry to the settlement worker",
		PRBody:         "Adds bounded retries around the settlement call.",
		PRAuthor:       "octocat",
		PRState:        "open",
		BaseSHA:        "1111111111111111111111111111111111111111",
		HeadSHA:        "2222222222222222222222222222222222222222",
		Diff: strings.Join([]string{
			"diff --git a/main.go b/main.go",
			"index 1111111..2222222 100644",
			"--- a/main.go",
			"+++ b/main.go",
			"@@ -10,4 +10,6 @@ func main() {",
			" 	if err != nil {",
			" 		log.Fatal(err)",
			" 	}",
			"+	// retry the settlement call",
			"+	settle(ctx)",
			" 	defer conn.Close()",
			"",
			"diff --git a/service.go b/service.go",
			"index 3333333..4444444 100644",
			"--- a/service.go",
			"+++ b/service.go",
			"@@ -40,4 +40,5 @@ func settle(ctx context.Context) error {",
			" 	if err != nil {",
			" 		return err",
			" 	}",
			"+	return nil",
			" }",
		}, "\n"),
		Files: []FileEntry{
			{Filename: "main.go", Additions: 2, Deletions: 0, Status: "modified"},
			{Filename: "service.go", Additions: 1, Deletions: 0, Status: "modified"},
		},
		Contents: map[string]string{
			"main.go":    "package main\n\nfunc main() {\n\tsettle(nil)\n}\n",
			"service.go": "package main\n\nfunc settle(ctx context.Context) error {\n\treturn nil\n}\n",
		},
	}

	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Server.Close)
	f.Tarball = TarGz(t, f.Contents)
	return f
}

// TarGz builds the repository archive a GitHub tarball endpoint serves: one
// gzipped tar with a single top directory.
func TarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := []byte(files[name])
		header := &tar.Header{
			Name:     "acme-payments-2222222/" + name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing the test archive header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("writing the test archive body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the test archive: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the test archive: %v", err)
	}
	return buf.Bytes()
}

// URL is the API root, suitable for both go-github and ghinstallation.
func (f *FakeGitHub) URL() string { return f.Server.URL }

// Reviews returns a copy of the reviews submitted so far.
func (f *FakeGitHub) SubmittedReviews() []SubmittedReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SubmittedReview(nil), f.Reviews...)
}

// RequestLog returns the methods and paths seen so far.
func (f *FakeGitHub) RequestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Requests...)
}

// CheckRunSnapshot returns a copy of the check runs the fake has seen.
func (f *FakeGitHub) CheckRunSnapshot() []CheckRunEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CheckRunEntry(nil), f.CheckRuns...)
}

// MintCount returns how many installation tokens were minted.
func (f *FakeGitHub) MintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Mints
}

func (f *FakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.handleMint(w)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.handleApp(w)
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath():
		f.handlePull(w, r)
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/files":
		f.writeJSON(w, http.StatusOK, f.Files)
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/reviews":
		f.writeJSON(w, http.StatusOK, f.reviewsJSON())
	case r.Method == http.MethodPost && r.URL.Path == f.repoPath()+"/check-runs":
		f.handleCheckRunCreate(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, f.repoPath()+"/check-runs/"):
		f.handleCheckRunUpdate(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/commits/") &&
		strings.HasSuffix(r.URL.Path, "/check-runs"):
		f.handleCheckRunList(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		f.handleGraphQL(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/compare/"):
		w.Header().Set("Content-Type", "application/vnd.github.v3.diff")
		_, _ = w.Write([]byte(f.SinceDiff))
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/comments":
		f.writeJSON(w, http.StatusOK, f.Comments)
	case r.Method == http.MethodPost && r.URL.Path == f.pullPath()+"/comments":
		f.handlePostReviewComment(w, r)
	case r.Method == http.MethodGet && r.URL.Path == f.issuePath()+"/comments":
		f.writeJSON(w, http.StatusOK, f.IssueComments)
	case r.Method == http.MethodPost && r.URL.Path == f.issuePath()+"/comments":
		f.handlePostIssueComment(w, r)
	case r.Method == http.MethodPost && r.URL.Path == f.issuePath()+"/reactions":
		f.handleReaction(w, r, "pull_request", 0)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reactions") &&
		strings.HasPrefix(r.URL.Path, f.repoPath()+"/issues/comments/"):
		// A conversation comment is addressed by its own id, without the pull
		// request number the comment belongs to.
		f.handleReaction(w, r, "issue_comment", f.commentID(r.URL.Path, f.repoPath()+"/issues/comments/"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reactions") &&
		strings.HasPrefix(r.URL.Path, f.repoPath()+"/pulls/comments/"):
		f.handleReaction(w, r, "review_comment", f.commentID(r.URL.Path, f.repoPath()+"/pulls/comments/"))
	case r.Method == http.MethodPost && r.URL.Path == f.pullPath()+"/reviews":
		f.handleReview(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/archive/rumil.tar.gz":
		f.mu.Lock()
		body := append([]byte(nil), f.Tarball...)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/tarball/"):
		f.mu.Lock()
		status := f.TarballStatus
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"archive unavailable"}`, status)
			return
		}
		http.Redirect(w, r, f.Server.URL+"/archive/rumil.tar.gz", http.StatusFound)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/git/trees/"):
		f.handleTree(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/contents/"):
		f.handleContents(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/git/blobs/"):
		f.handleBlob(w, r)
	default:
		http.Error(w, fmt.Sprintf(`{"message":"unexpected %s %s"}`, r.Method, r.URL.Path), http.StatusNotFound)
	}
}

// AddedReactions returns a copy of the reactions seen so far.
func (f *FakeGitHub) AddedReactions() []Reaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Reaction(nil), f.Reactions...)
}

// handleReaction records one acknowledgement, and answers with the reaction the
// API would return.
func (f *FakeGitHub) handleReaction(w http.ResponseWriter, r *http.Request, target string, id int64) {
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	status := f.ReactionStatus
	f.Reactions = append(f.Reactions, Reaction{Target: target, ID: id, Content: body.Content})
	sequence := len(f.Reactions)
	f.mu.Unlock()

	if status != 0 {
		http.Error(w, `{"message":"the reaction was refused"}`, status)
		return
	}
	f.writeJSON(w, http.StatusCreated, map[string]any{"id": sequence, "content": body.Content})
}

// commentID reads the comment id out of a reactions path, which is
// ".../comments/<id>/reactions".
func (f *FakeGitHub) commentID(path, prefix string) int64 {
	rest := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/reactions")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func (f *FakeGitHub) handleMint(w http.ResponseWriter) {
	f.mu.Lock()
	f.Mints++
	status := f.MintStatus
	f.mu.Unlock()

	if status != 0 {
		f.writeJSON(w, status, map[string]string{"message": "Not Found"})
		return
	}
	f.writeJSON(w, http.StatusCreated, map[string]string{
		"token":      "ghs_test_installation_token",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

// handleApp serves the installation's own record, which is the read that answers
// without an installation token and carries both the App's slug and the
// permissions the tokens it mints will hold.
func (f *FakeGitHub) handleApp(w http.ResponseWriter) {
	f.mu.Lock()
	slug, permissions, status := f.AppSlug, f.AppPermissions, f.AppStatus
	f.mu.Unlock()

	if status != 0 {
		f.writeJSON(w, status, map[string]string{"message": "Not Found"})
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"id": 98765432, "app_id": 123456, "app_slug": slug, "permissions": permissions,
	})
}

func (f *FakeGitHub) handlePull(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "diff") {
		f.mu.Lock()
		status := f.DiffStatus
		diff := f.Diff
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"Sorry, the diff exceeded the maximum number of files (300). Consider using 'List pull requests files' API or locally cloning the repository instead."}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.v3.diff")
		_, _ = w.Write([]byte(diff))
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"number": f.Number,
		"title":  f.PRTitle,
		"body":   f.PRBody,
		"state":  f.PRState,
		"user":   map[string]string{"login": f.PRAuthor, "type": "User"},
		"base":   map[string]string{"sha": f.BaseSHA},
		"head":   map[string]string{"sha": f.HeadSHA},
	})
}

// inlineContentLimit mirrors the contents API, which inlines a file body up to
// 1 MB and returns an empty body with encoding "none" above that.
const inlineContentLimit = 1 << 20

func (f *FakeGitHub) handleContents(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, f.repoPath()+"/contents/")
	ref := r.URL.Query().Get("ref")

	f.mu.Lock()
	source := f.Contents
	if byRef, ok := f.ContentsByRef[ref]; ok {
		source = byRef
	}
	content, ok := source[path]
	f.mu.Unlock()

	if !ok {
		f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}

	sha := blobSHA(content)
	entry := map[string]any{
		"type": "file",
		"name": pathpkg.Base(path),
		"path": path,
		"size": len(content),
		"sha":  sha,
	}
	if len(content) > inlineContentLimit {
		f.mu.Lock()
		if f.Blobs == nil {
			f.Blobs = map[string]string{}
		}
		f.Blobs[sha] = content
		f.mu.Unlock()
		entry["encoding"] = "none"
		entry["content"] = ""
	} else {
		// The contents API returns files inline, base64 encoded.
		entry["encoding"] = "base64"
		entry["content"] = base64.StdEncoding.EncodeToString([]byte(content))
	}
	f.writeJSON(w, http.StatusOK, entry)
}

func (f *FakeGitHub) handleBlob(w http.ResponseWriter, r *http.Request) {
	sha := strings.TrimPrefix(r.URL.Path, f.repoPath()+"/git/blobs/")

	f.mu.Lock()
	content, ok := f.Blobs[sha]
	f.mu.Unlock()

	if !ok {
		f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"sha":      sha,
		"size":     len(content),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	})
}

// blobSHA is the git object hash of content: the API reports it on the contents
// entry and keys the blob endpoint by it.
func blobSHA(content string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	return hex.EncodeToString(sum[:])
}

func (f *FakeGitHub) handleReview(w http.ResponseWriter, r *http.Request) {
	var review SubmittedReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	status := 0
	if f.ReviewStatus != 0 && f.reviewFailures == 0 {
		status = f.ReviewStatus
		f.reviewFailures++
	}
	reviewID := int64(len(f.Reviews)) + 1
	if status == 0 {
		f.Reviews = append(f.Reviews, review)
		// GitHub serves the inline comments of a created review on the next
		// listing, each carrying the review id that created it.
		for i, comment := range review.Comments {
			entry := ReviewComment(3000+int64(len(f.Comments))+int64(i), 0, f.botLogin(), comment.Body)
			entry.Path = comment.Path
			entry.Line = comment.Line
			entry.PullRequestReviewID = reviewID
			f.Comments = append(f.Comments, entry)
		}
	}
	f.mu.Unlock()

	if status != 0 {
		f.writeJSON(w, status, map[string]string{"message": "Unprocessable Entity"})
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"id":    reviewID,
		"state": reviewState(review.Event),
		"body":  review.Body,
	})
}

func reviewState(event string) string {
	switch event {
	case "APPROVE":
		return "APPROVED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	default:
		return "COMMENTED"
	}
}

func (f *FakeGitHub) repoPath() string {
	return "/repos/" + f.Owner + "/" + f.Repo
}

func (f *FakeGitHub) pullPath() string {
	return f.repoPath() + "/pulls/" + strconv.Itoa(f.Number)
}

func (f *FakeGitHub) issuePath() string {
	return f.repoPath() + "/issues/" + strconv.Itoa(f.Number)
}

// PostedComments returns a copy of the comments Rumil posted.
func (f *FakeGitHub) PostedComments() []PostedComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PostedComment(nil), f.Posted...)
}

// handlePostReviewComment answers the reply endpoint, which GitHub serves as a
// comment carrying in_reply_to.
func (f *FakeGitHub) handlePostReviewComment(w http.ResponseWriter, r *http.Request) {
	var post PostedComment
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&post); err != nil {
		f.writeJSON(w, http.StatusBadRequest, map[string]string{"message": "malformed comment"})
		return
	}

	f.mu.Lock()
	f.Posted = append(f.Posted, post)
	// GitHub serves a posted comment on the next listing, and a retry that
	// cannot see its own answer would post it twice.
	entry := ReviewComment(1000+int64(len(f.Posted)), post.InReplyTo, f.botLogin(), post.Body)
	f.Comments = append(f.Comments, entry)
	f.mu.Unlock()

	f.writeJSON(w, http.StatusCreated, map[string]any{"id": entry.ID, "body": post.Body})
}

func (f *FakeGitHub) handlePostIssueComment(w http.ResponseWriter, r *http.Request) {
	var post struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&post); err != nil {
		f.writeJSON(w, http.StatusBadRequest, map[string]string{"message": "malformed comment"})
		return
	}

	f.mu.Lock()
	f.Posted = append(f.Posted, PostedComment{Body: post.Body})
	entry := ReviewComment(2000+int64(len(f.Posted)), 0, f.botLogin(), post.Body)
	f.IssueComments = append(f.IssueComments, entry)
	f.mu.Unlock()

	f.writeJSON(w, http.StatusCreated, map[string]any{"id": entry.ID, "body": post.Body})
}

// botLogin is the login a recorded post is attributed to.
func (f *FakeGitHub) botLogin() string {
	if f.BotLogin != "" {
		return f.BotLogin
	}
	return "rumil[bot]"
}

// reviewsJSON renders the stored reviews in the shape of the REST endpoint.
// reviewsJSON renders the reviews the endpoint serves: the ones the fixture
// already holds, then the ones submitted through this fake, which GitHub would
// serve the same way, each against the head commit it was submitted on.
func (f *FakeGitHub) reviewsJSON() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	submittedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	entry := func(id int64, author, state, body, commit string) map[string]any {
		return map[string]any{
			"id":           id,
			"state":        state,
			"body":         body,
			"commit_id":    commit,
			"user":         map[string]string{"login": author, "type": "Bot"},
			"submitted_at": submittedAt,
		}
	}

	out := make([]map[string]any, 0, len(f.ExistingReviews)+len(f.Reviews))
	for _, review := range f.ExistingReviews {
		out = append(out, entry(review.ID, review.Author, review.State, review.Body, review.CommitID))
	}
	for i, review := range f.Reviews {
		out = append(out, entry(int64(i+1), f.botLogin(), reviewState(review.Event), review.Body, f.HeadSHA))
	}
	return out
}

// handleGraphQL answers the two operations Rumil issues. It reads the query
// text rather than parsing the document, which is enough to serve a fixture.
func (f *FakeGitHub) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		f.writeJSON(w, http.StatusBadRequest, map[string]string{"message": "malformed graphql request"})
		return
	}

	f.mu.Lock()
	errors := append([]string(nil), f.GraphQLErrors...)
	f.mu.Unlock()

	if len(errors) > 0 {
		entries := make([]map[string]string, 0, len(errors))
		for _, message := range errors {
			entries = append(entries, map[string]string{"message": message, "type": "FORBIDDEN"})
		}
		f.writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": entries})
		return
	}

	switch {
	case strings.Contains(body.Query, "resolveReviewThread"):
		threadID, _ := body.Variables["threadId"].(string)
		f.mu.Lock()
		refusal := f.ResolveError
		f.mu.Unlock()
		if refusal != "" {
			f.writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]string{
				{"message": refusal, "type": "FORBIDDEN"},
			}})
			return
		}
		f.mu.Lock()
		f.ResolvedThreads = append(f.ResolvedThreads, threadID)
		for i := range f.Threads {
			if f.Threads[i].ID == threadID {
				f.Threads[i].IsResolved = true
			}
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"resolveReviewThread": map[string]any{"thread": map[string]any{"id": threadID, "isResolved": true}},
		}})
	case strings.Contains(body.Query, "reviewThreads"):
		f.writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewThreads": f.threadPage(body.Variables),
				},
			},
		}})
	default:
		f.writeJSON(w, http.StatusOK, map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": "the query is not one this fake serves", "type": "BAD_REQUEST"}},
		})
	}
}

// threadPage serves one page of threads. ThreadPageSize 0 serves every thread
// in one page, which is what a fixture of a few threads looks like; a smaller
// size makes the caller follow endCursor, which is what a test of the paging
// loop needs.
func (f *FakeGitHub) threadPage(variables map[string]any) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	start := 0
	if cursor, _ := variables["cursor"].(string); cursor != "" {
		for i, thread := range f.Threads {
			if thread.ID == cursor {
				start = i + 1
				break
			}
		}
	}
	size := f.ThreadPageSize
	if size <= 0 || start+size > len(f.Threads) {
		size = len(f.Threads) - start
	}
	end := start + size

	hasNext := end < len(f.Threads)
	endCursor := ""
	if hasNext && end > start {
		endCursor = f.Threads[end-1].ID
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
		"nodes":    f.threadNodesLocked(f.Threads[start:end]),
	}
}

// threadNodesLocked renders threads in the shape of the GraphQL query. The
// caller holds the lock.
func (f *FakeGitHub) threadNodesLocked(threads []ThreadEntry) []map[string]any {
	nodes := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		comments := make([]map[string]any, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			comments = append(comments, map[string]any{
				"databaseId": comment.ID,
				"author":     map[string]string{"login": comment.User.Login},
				"body":       comment.Body,
				"path":       thread.Path,
				"line":       thread.Line,
				"createdAt":  comment.CreatedAt.Format(time.RFC3339),
			})
		}
		nodes = append(nodes, map[string]any{
			"id":         thread.ID,
			"isResolved": thread.IsResolved,
			"isOutdated": thread.IsOutdated,
			"path":       thread.Path,
			"line":       thread.Line,
			"comments": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false},
				"nodes":    comments,
			},
		})
	}
	return nodes
}

// Resolved returns the thread ids the mutation was called with.
func (f *FakeGitHub) Resolved() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ResolvedThreads...)
}

func (f *FakeGitHub) handleTree(w http.ResponseWriter, r *http.Request) {
	sha := pathpkg.Base(r.URL.Path)
	f.mu.Lock()
	names := make([]string, 0, len(f.Contents))
	for name := range f.Contents {
		names = append(names, name)
	}
	sizes := make(map[string]int, len(names))
	for _, name := range names {
		sizes[name] = len(f.Contents[name])
	}
	f.mu.Unlock()
	sort.Strings(names)

	tree := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tree = append(tree, map[string]any{
			"path": name, "mode": "100644", "type": "blob", "size": sizes[name],
		})
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"sha": sha, "truncated": false, "tree": tree,
	})
}

// checkRunRequest is the part of the create and update bodies the fake reads.
type checkRunRequest struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

func (f *FakeGitHub) handleCheckRunCreate(w http.ResponseWriter, r *http.Request) {
	if f.refuseCheckRun(w) {
		return
	}
	var request checkRunRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	entry := CheckRunEntry{
		ID:         int64(len(f.CheckRuns)) + 1,
		Name:       request.Name,
		HeadSHA:    request.HeadSHA,
		Status:     request.Status,
		Conclusion: request.Conclusion,
		DetailsURL: request.DetailsURL,
		Summary:    checkRunSummary(request.Output),
	}
	f.CheckRuns = append(f.CheckRuns, entry)
	f.mu.Unlock()

	f.writeJSON(w, http.StatusCreated, entry)
}

func (f *FakeGitHub) handleCheckRunUpdate(w http.ResponseWriter, r *http.Request) {
	if f.refuseCheckRun(w) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, f.repoPath()+"/check-runs/"), 10, 64)
	if err != nil {
		http.Error(w, "bad check run id", http.StatusBadRequest)
		return
	}
	var request checkRunRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	for i := range f.CheckRuns {
		if f.CheckRuns[i].ID != id {
			continue
		}
		f.CheckRuns[i].Status = request.Status
		f.CheckRuns[i].Conclusion = request.Conclusion
		if request.DetailsURL != "" {
			f.CheckRuns[i].DetailsURL = request.DetailsURL
		}
		if summary := checkRunSummary(request.Output); summary != "" {
			f.CheckRuns[i].Summary = summary
		}
		entry := f.CheckRuns[i]
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, entry)
		return
	}
	f.mu.Unlock()

	f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (f *FakeGitHub) handleCheckRunList(w http.ResponseWriter, r *http.Request) {
	if f.refuseCheckRun(w) {
		return
	}
	ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, f.repoPath()+"/commits/"), "/check-runs")
	name := r.URL.Query().Get("check_name")

	f.mu.Lock()
	runs := make([]CheckRunEntry, 0, len(f.CheckRuns))
	for _, entry := range f.CheckRuns {
		if entry.HeadSHA == ref && (name == "" || entry.Name == name) {
			runs = append(runs, entry)
		}
	}
	f.mu.Unlock()

	f.writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}

func (f *FakeGitHub) refuseCheckRun(w http.ResponseWriter) bool {
	if f.CheckStatus == 0 {
		return false
	}
	f.writeJSON(w, f.CheckStatus, map[string]string{"message": "Resource not accessible by integration"})
	return true
}

func checkRunSummary(output *struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}) string {
	if output == nil {
		return ""
	}
	return output.Summary
}

func (f *FakeGitHub) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// TestPrivateKeyPEM generates an RSA key at test time. No key material is ever
// committed to the repository.
func TestPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}
