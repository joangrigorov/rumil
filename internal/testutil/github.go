// Package testutil provides the hermetic doubles the verification matrix
// needs: a fake GitHub REST API, a fake OpenAI-compatible chat endpoint, and
// test key material. Nothing here talks to the network outside httptest.
package testutil

import (
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// FileEntry is one entry in the fake pull request file list.
type FileEntry struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
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

	Diff     string
	Files    []FileEntry
	Contents map[string]string
	// ContentsByRef overrides Contents for a specific ref, so a test can serve
	// different text on the base SHA and the head SHA.
	ContentsByRef map[string]map[string]string
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
	// Posted collects the comments Rumil posted through the reply and
	// conversation endpoints.
	Posted []PostedComment
	// BotLogin is the login the fake reports for the comments it records a post
	// as, so a later listing sees them exactly as GitHub would serve them.
	BotLogin string

	// MintStatus, when non-zero, is returned by the installation token
	// endpoint instead of a token (404 simulates an App that is not installed).
	MintStatus int
	// ReviewStatus, when non-zero, is returned by the first review POST.
	ReviewStatus int

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
		Owner:    "acme",
		Repo:     "payments",
		Number:   42,
		PRTitle:  "Add retry to the settlement worker",
		PRBody:   "Adds bounded retries around the settlement call.",
		PRAuthor: "octocat",
		PRState:  "open",
		BaseSHA:  "1111111111111111111111111111111111111111",
		HeadSHA:  "2222222222222222222222222222222222222222",
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
	return f
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
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath():
		f.handlePull(w, r)
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/files":
		f.writeJSON(w, http.StatusOK, f.Files)
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/reviews":
		f.writeJSON(w, http.StatusOK, f.reviewsJSON())
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
	case r.Method == http.MethodPost && r.URL.Path == f.pullPath()+"/reviews":
		f.handleReview(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/contents/"):
		f.handleContents(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/git/blobs/"):
		f.handleBlob(w, r)
	default:
		http.Error(w, fmt.Sprintf(`{"message":"unexpected %s %s"}`, r.Method, r.URL.Path), http.StatusNotFound)
	}
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

func (f *FakeGitHub) handlePull(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "diff") {
		w.Header().Set("Content-Type", "application/vnd.github.v3.diff")
		_, _ = w.Write([]byte(f.Diff))
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
	if status == 0 {
		f.Reviews = append(f.Reviews, review)
	}
	f.mu.Unlock()

	if status != 0 {
		f.writeJSON(w, status, map[string]string{"message": "Unprocessable Entity"})
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"id":    len(f.SubmittedReviews()),
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
					"reviewThreads": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes":    f.threadNodes(),
					},
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

// threadNodes renders the stored threads in the shape of the GraphQL query.
func (f *FakeGitHub) threadNodes() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	nodes := make([]map[string]any, 0, len(f.Threads))
	for _, thread := range f.Threads {
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
