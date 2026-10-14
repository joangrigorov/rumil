// Package testutil provides the hermetic doubles the verification matrix
// needs: a fake GitHub REST API, a fake OpenAI-compatible chat endpoint, and
// test key material. Nothing here talks to the network outside httptest.
package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
	Comments []CommentEntry

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
	case r.Method == http.MethodGet && r.URL.Path == f.pullPath()+"/comments":
		f.writeJSON(w, http.StatusOK, f.Comments)
	case r.Method == http.MethodPost && r.URL.Path == f.pullPath()+"/reviews":
		f.handleReview(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, f.repoPath()+"/contents/"):
		f.handleContents(w, r)
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

func (f *FakeGitHub) handleContents(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, f.repoPath()+"/contents/")
	content, ok := f.Contents[path]
	if !ok {
		f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	// The contents API returns files inline, base64 encoded.
	f.writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"name":     pathpkg.Base(path),
		"path":     path,
		"encoding": "base64",
		"size":     len(content),
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	})
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
