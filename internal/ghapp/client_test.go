package ghapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v69/github"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const sampleDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -10,4 +10,6 @@ func main() {
 	if err != nil {
 		log.Fatal(err)
 	}
+	// retry the settlement call
+	settle(ctx)
 	defer conn.Close()
diff --git a/legacy.go b/legacy.go
deleted file mode 100644
--- a/legacy.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package legacy
-
-func unused() {}
`

func TestParseUnifiedDiff(t *testing.T) {
	diff, err := ParseUnifiedDiff(sampleDiff)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff returned error: %v", err)
	}

	if len(diff.Files) != 2 {
		t.Fatalf("parsed %d files, want 2", len(diff.Files))
	}

	main := diff.File("main.go")
	if main == nil {
		t.Fatal("main.go not found in the diff")
	}
	if main.Additions != 2 || main.Deletions != 0 {
		t.Errorf("main.go additions/deletions = %d/%d, want 2/0", main.Additions, main.Deletions)
	}

	// Added lines carry only a new line number.
	added := main.Lines[3]
	if added.Kind != LineAdded || added.NewLine != 13 || added.OldLine != 0 {
		t.Errorf("first added line = %+v, want added with NEW:13 and no OLD line", added)
	}
	// Context lines carry both.
	context := main.Lines[0]
	if context.Kind != LineContext || context.OldLine != 10 || context.NewLine != 10 {
		t.Errorf("first context line = %+v, want context OLD:10 NEW:10", context)
	}

	legacy := diff.File("legacy.go")
	if legacy == nil {
		t.Fatal("legacy.go not found in the diff")
	}
	if legacy.Additions != 0 || legacy.Deletions != 3 {
		t.Errorf("legacy.go additions/deletions = %d/%d, want 0/3", legacy.Additions, legacy.Deletions)
	}
	if legacy.Lines[0].Kind != LineRemoved || legacy.Lines[0].OldLine != 1 || legacy.Lines[0].NewLine != 0 {
		t.Errorf("first removed line = %+v, want removed with OLD:1 and no NEW line", legacy.Lines[0])
	}
}

func TestHasNewLine(t *testing.T) {
	diff, err := ParseUnifiedDiff(sampleDiff)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff returned error: %v", err)
	}

	tests := []struct {
		path string
		line int
		want bool
	}{
		{"main.go", 13, true},   // added line
		{"main.go", 14, true},   // added line
		{"main.go", 10, true},   // context line
		{"main.go", 999, false}, // outside the hunk
		{"main.go", 1, false},   // before the hunk
		{"legacy.go", 1, false},
		{"missing.go", 1, false},
	}

	for _, tt := range tests {
		if got := diff.HasNewLine(tt.path, tt.line); got != tt.want {
			t.Errorf("HasNewLine(%q, %d) = %v, want %v", tt.path, tt.line, got, tt.want)
		}
	}
}

func TestAnnotateUsesExplicitLineNumbers(t *testing.T) {
	diff, err := ParseUnifiedDiff(sampleDiff)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff returned error: %v", err)
	}

	annotated := diff.Annotate("main.go")
	for _, want := range []string{
		"file: main.go (+2/-0)",
		"[OLD:10 NEW:10]",
		"[OLD:- NEW:13] + \t// retry the settlement call",
		"[OLD:13 NEW:15]",
		"\tdefer conn.Close()",
	} {
		if !strings.Contains(annotated, want) {
			t.Errorf("annotation is missing %q:\n%s", want, annotated)
		}
	}
	if strings.Contains(annotated, "legacy.go") {
		t.Errorf("annotating main.go included another file:\n%s", annotated)
	}
}

func TestAnnotatedPagesSplitOnLineBoundaries(t *testing.T) {
	diff, err := ParseUnifiedDiff(sampleDiff)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff returned error: %v", err)
	}

	pages := diff.AnnotatedPages(120)
	if len(pages) < 2 {
		t.Fatalf("got %d pages, want the diff split into several", len(pages))
	}
	for i, page := range pages {
		if len(page) > 120 {
			t.Errorf("page %d is %d bytes, want at most 120", i+1, len(page))
		}
	}
	joined := strings.Join(pages, "\n")
	if !strings.Contains(joined, "NEW:13") || !strings.Contains(joined, "legacy.go") {
		t.Errorf("pages lost content:\n%s", joined)
	}
}

func TestParseUnifiedDiffRejectsMalformedHunkHeader(t *testing.T) {
	_, err := ParseUnifiedDiff("--- a/x\n+++ b/x\n@@ not a hunk header @@\n")
	if err == nil {
		t.Fatal("ParseUnifiedDiff succeeded on a malformed hunk header, want an error")
	}
}

func TestParseUnifiedDiffHandlesBinarySections(t *testing.T) {
	diff, err := ParseUnifiedDiff("--- a/logo.png\n+++ b/logo.png\nBinary files a/logo.png and b/logo.png differ\n")
	if err != nil {
		t.Fatalf("ParseUnifiedDiff returned error: %v", err)
	}
	if !diff.Files[0].Binary {
		t.Errorf("file = %+v, want it flagged as binary", diff.Files[0])
	}
}

// newTestClient wires a plain go-github client to the fake API.
func newTestClient(t *testing.T, gh *testutil.FakeGitHub) *Client {
	t.Helper()

	client := github.NewClient(nil)
	base, err := url.Parse(gh.URL() + "/")
	if err != nil {
		t.Fatalf("parsing the fake API URL: %v", err)
	}
	client.BaseURL = base

	return NewClient(client, gh.Owner, gh.Repo, gh.Number)
}

func TestClientMetadata(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	meta, err := client.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata returned error: %v", err)
	}
	if meta.Title != gh.PRTitle || meta.Author != gh.PRAuthor {
		t.Errorf("metadata = %+v, want the fake pull request header", meta)
	}
	if meta.BaseSHA != gh.BaseSHA || meta.HeadSHA != gh.HeadSHA {
		t.Errorf("SHAs = %s/%s, want %s/%s", meta.BaseSHA, meta.HeadSHA, gh.BaseSHA, gh.HeadSHA)
	}
	if len(meta.ChangedFiles) != 2 || meta.ChangedFiles[0].Path != "main.go" {
		t.Errorf("changed files = %+v, want the two fixture files", meta.ChangedFiles)
	}

	// Metadata is memoized: a second call must not hit the API again.
	before := len(gh.RequestLog())
	if _, err := client.Metadata(context.Background()); err != nil {
		t.Fatalf("second Metadata call returned error: %v", err)
	}
	if got := len(gh.RequestLog()); got != before {
		t.Errorf("requests grew from %d to %d, want the metadata cached", before, got)
	}
}

func TestClientDiffIsMemoized(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	first, err := client.Diff(context.Background())
	if err != nil {
		t.Fatalf("Diff returned error: %v", err)
	}
	requests := len(gh.RequestLog())

	second, err := client.Diff(context.Background())
	if err != nil {
		t.Fatalf("second Diff returned error: %v", err)
	}
	if first != second {
		t.Error("Diff returned a different parsed diff on the second call")
	}
	if got := len(gh.RequestLog()); got != requests {
		t.Errorf("requests grew from %d to %d, want the diff cached", requests, got)
	}
}

func TestClientFileContents(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Contents["big.go"] = strings.Repeat("line\n", 600)
	gh.Contents["binary.bin"] = "PK\x00\x01binary"
	client := newTestClient(t, gh)

	t.Run("defaults to the head SHA and returns the requested range", func(t *testing.T) {
		gh.Contents["ranged.go"] = "one\ntwo\nthree\nfour\n"
		got, err := client.FileContents(context.Background(), "ranged.go", "", 2, 3)
		if err != nil {
			t.Fatalf("FileContents returned error: %v", err)
		}
		if got != "two\nthree" {
			t.Errorf("content = %q, want \"two\\nthree\"", got)
		}
	})

	t.Run("caps the read at 500 lines", func(t *testing.T) {
		got, err := client.FileContents(context.Background(), "big.go", "", 1, 0)
		if err != nil {
			t.Fatalf("FileContents returned error: %v", err)
		}
		if lines := strings.Count(got, "\n"); lines != MaxFileReadLines {
			t.Errorf("returned %d lines, want %d", lines, MaxFileReadLines)
		}
		if !strings.Contains(got, "truncated at 500 lines") {
			t.Errorf("truncation marker missing:\n%s", got[len(got)-120:])
		}
	})

	t.Run("rejects a start line past the end", func(t *testing.T) {
		if _, err := client.FileContents(context.Background(), "ranged.go", "", 900, 901); err == nil {
			t.Fatal("FileContents succeeded past the end of the file, want an error")
		}
	})

	t.Run("rejects binary files", func(t *testing.T) {
		_, err := client.FileContents(context.Background(), "binary.bin", "", 0, 0)
		if err == nil || !strings.Contains(err.Error(), "binary") {
			t.Fatalf("error = %v, want a binary file rejection", err)
		}
	})
}

func TestClientExistingReviewComments(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	entry := testutil.CommentEntry{ID: 11, Path: "service.go", Line: 44, Body: "already said this"}
	entry.User.Login = "rumil[bot]"
	gh.Comments = []testutil.CommentEntry{entry}

	client := newTestClient(t, gh)

	comments, err := client.ExistingReviewComments(context.Background())
	if err != nil {
		t.Fatalf("ExistingReviewComments returned error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	if comments[0].Path != "service.go" || comments[0].Line != 44 || comments[0].Author != "rumil[bot]" {
		t.Errorf("comment = %+v, want the fixture comment with its author", comments[0])
	}
}

func TestSubmitReview(t *testing.T) {
	ctx := context.Background()

	t.Run("keeps valid inline comments and moves the rest into the summary", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		client := newTestClient(t, gh)
		diff, err := client.Diff(ctx)
		if err != nil {
			t.Fatalf("Diff returned error: %v", err)
		}

		outcome, err := client.SubmitReview(ctx, diff, ReviewRequest{
			Event:       EventComment,
			SummaryBody: "Summary.",
			Comments: []ReviewComment{
				{Path: "main.go", Line: 13, Body: "valid"},
				{Path: "main.go", Line: 999, Body: "outside the diff"},
				{Path: "missing.go", Line: 1, Body: "unknown file"},
			},
		})
		if err != nil {
			t.Fatalf("SubmitReview returned error: %v", err)
		}
		if len(outcome.MovedToSummary) != 2 {
			t.Errorf("moved %d comments into the summary, want 2", len(outcome.MovedToSummary))
		}

		reviews := gh.SubmittedReviews()
		if len(reviews[0].Comments) != 1 {
			t.Fatalf("inline comments = %d, want 1", len(reviews[0].Comments))
		}
		for _, want := range []string{"[main.go:999]", "[missing.go:1]"} {
			if !strings.Contains(reviews[0].Body, want) {
				t.Errorf("summary %q is missing %q", reviews[0].Body, want)
			}
		}
	})

	t.Run("skips a comment that already exists", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		entry := testutil.CommentEntry{ID: 3, Path: "main.go", Line: 13, Body: "same body"}
		entry.User.Login = "rumil[bot]"
		gh.Comments = []testutil.CommentEntry{entry}

		client := newTestClient(t, gh)
		diff, _ := client.Diff(ctx)

		outcome, err := client.SubmitReview(ctx, diff, ReviewRequest{
			Event:       EventComment,
			SummaryBody: "Summary.",
			Comments:    []ReviewComment{{Path: "main.go", Line: 13, Body: "same body"}},
		})
		if err != nil {
			t.Fatalf("SubmitReview returned error: %v", err)
		}
		if len(outcome.SkippedDuplicates) != 1 {
			t.Errorf("skipped %d duplicates, want 1", len(outcome.SkippedDuplicates))
		}
		if len(gh.SubmittedReviews()[0].Comments) != 0 {
			t.Errorf("inline comments = %d, want the duplicate suppressed", len(gh.SubmittedReviews()[0].Comments))
		}
	})

	t.Run("falls back to COMMENT when GitHub rejects a stronger event", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.ReviewStatus = 422
		client := newTestClient(t, gh)
		diff, _ := client.Diff(ctx)

		outcome, err := client.SubmitReview(ctx, diff, ReviewRequest{
			Event:       EventRequestChanges,
			SummaryBody: "Blocking defect.",
			Comments:    []ReviewComment{{Path: "main.go", Line: 13, Body: "blocking"}},
		})
		if err != nil {
			t.Fatalf("SubmitReview returned error: %v", err)
		}
		if !outcome.Fallback || outcome.Event != EventComment || outcome.RequestedEvent != EventRequestChanges {
			t.Errorf("outcome = %+v, want a fallback to COMMENT", outcome)
		}

		reviews := gh.SubmittedReviews()
		if len(reviews) != 1 || reviews[0].Event != EventComment {
			t.Fatalf("reviews = %+v, want one COMMENT review", reviews)
		}
		if !strings.Contains(reviews[0].Body, "REQUEST_CHANGES") {
			t.Errorf("summary %q does not record the intended event", reviews[0].Body)
		}
	})

	t.Run("surfaces an error when COMMENT itself is rejected", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.ReviewStatus = 422
		client := newTestClient(t, gh)
		diff, _ := client.Diff(ctx)

		if _, err := client.SubmitReview(ctx, diff, ReviewRequest{Event: EventComment, SummaryBody: "x"}); err == nil {
			t.Fatal("SubmitReview succeeded, want the API error surfaced")
		}
	})
}

func TestNewAppAuthRejectsInvalidPrivateKey(t *testing.T) {
	_, err := NewAppAuth(config.GitHub{AppID: 1, InstallationID: 2}, []byte("not a pem"), "")
	if !errors.Is(err, ErrInvalidPrivateKey) {
		t.Fatalf("error = %v, want it to wrap ErrInvalidPrivateKey", err)
	}
}

// The App's own record is the only source for the login people mention and the
// login its reviews are attributed to, and it is the one read that needs no
// installation token.
func TestSlugReadsTheAppsOwnRecord(t *testing.T) {
	t.Run("returns the slug the record carries", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.AppSlug = "rumil-testing"

		auth, err := NewAppAuth(config.GitHub{AppID: 123, InstallationID: 987}, testutil.TestPrivateKeyPEM(t), gh.URL())
		if err != nil {
			t.Fatalf("NewAppAuth returned error: %v", err)
		}

		slug, err := auth.Slug(context.Background())
		if err != nil {
			t.Fatalf("Slug returned error: %v", err)
		}
		if slug != "rumil-testing" {
			t.Errorf("slug = %q, want rumil-testing", slug)
		}
	})

	t.Run("surfaces a record that could not be read", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.AppStatus = http.StatusInternalServerError

		auth, err := NewAppAuth(config.GitHub{AppID: 123, InstallationID: 987}, testutil.TestPrivateKeyPEM(t), gh.URL())
		if err != nil {
			t.Fatalf("NewAppAuth returned error: %v", err)
		}

		if _, err := auth.Slug(context.Background()); err == nil {
			t.Fatal("Slug succeeded, want the API error surfaced")
		}
	})

	t.Run("refuses a record with no slug", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.AppSlug = ""

		auth, err := NewAppAuth(config.GitHub{AppID: 123, InstallationID: 987}, testutil.TestPrivateKeyPEM(t), gh.URL())
		if err != nil {
			t.Fatalf("NewAppAuth returned error: %v", err)
		}

		if _, err := auth.Slug(context.Background()); err == nil {
			t.Fatal("Slug succeeded, want an empty record refused")
		}
	})
}

func TestVerifyReportsMissingInstallation(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.MintStatus = 404

	auth, err := NewAppAuth(config.GitHub{AppID: 123, InstallationID: 987}, testutil.TestPrivateKeyPEM(t), gh.URL())
	if err != nil {
		t.Fatalf("NewAppAuth returned error: %v", err)
	}

	err = auth.Verify(context.Background())
	if !errors.Is(err, ErrAppNotInstalled) {
		t.Fatalf("error = %v, want it to wrap ErrAppNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "987") {
		t.Errorf("error = %v, want it to name the installation id", err)
	}

	// Once the App is installed, the same authenticator mints a token.
	gh.MintStatus = 0
	if err := auth.Verify(context.Background()); err != nil {
		t.Fatalf("Verify returned error after the App was installed: %v", err)
	}
	if gh.MintCount() != 2 {
		t.Errorf("mints = %d, want the initial failure and the successful retry", gh.MintCount())
	}
}

// ReviewGuidelines reads RUMIL.md at the repository root, and nothing else.
func TestReviewGuidelines(t *testing.T) {
	t.Run("reads the root file", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.Contents[GuidelinesPath] = "root policy"

		path, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), gh.BaseSHA)
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error: %v", err)
		}
		if path != GuidelinesPath || text != "root policy" {
			t.Errorf("got (%q, %q), want (%q, %q)", path, text, GuidelinesPath, "root policy")
		}
	})

	t.Run("ignores files with other names", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.Contents["REVIEW.md"] = "root policy"
		gh.Contents[".github/REVIEW.md"] = "github policy"
		gh.Contents["docs/REVIEW.md"] = "docs policy"

		path, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), gh.BaseSHA)
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error: %v", err)
		}
		if path != "" || text != "" {
			t.Errorf("got (%q, %q), want REVIEW.md at any path to be ignored", path, text)
		}
	})

	t.Run("no guidelines is not an error", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.Contents = map[string]string{}

		path, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), gh.BaseSHA)
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error for a repository without guidelines: %v", err)
		}
		if path != "" || text != "" {
			t.Errorf("got (%q, %q), want empty results", path, text)
		}
	})

	t.Run("reads the ref it is given", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.ContentsByRef = map[string]map[string]string{
			gh.BaseSHA: {GuidelinesPath: "trusted"},
			gh.HeadSHA: {GuidelinesPath: "untrusted"},
		}

		path, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), gh.HeadSHA)
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error: %v", err)
		}
		if path != GuidelinesPath || text != "untrusted" {
			t.Errorf("got (%q, %q), want the head SHA copy", path, text)
		}
	})

	t.Run("defaults to the base SHA", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.ContentsByRef = map[string]map[string]string{
			gh.BaseSHA: {GuidelinesPath: "trusted"},
			gh.HeadSHA: {GuidelinesPath: "untrusted"},
		}

		_, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), "")
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error: %v", err)
		}
		if text != "trusted" {
			t.Errorf("text = %q, want the base SHA copy", text)
		}
	})

	t.Run("truncates at the cap through the blob API", func(t *testing.T) {
		gh := testutil.NewFakeGitHub(t)
		gh.Contents[GuidelinesPath] = strings.Repeat("a", 5<<20)

		_, text, err := newTestClient(t, gh).ReviewGuidelines(context.Background(), gh.BaseSHA)
		if err != nil {
			t.Fatalf("ReviewGuidelines returned error: %v", err)
		}
		if !strings.HasPrefix(text, strings.Repeat("a", MaxGuidelinesBytes)) {
			t.Errorf("the guidelines do not start with %d bytes of the file", MaxGuidelinesBytes)
		}
		wantMarker := fmt.Sprintf("\n\n[truncated at %d bytes: the guidelines file is %d bytes]", MaxGuidelinesBytes, 5<<20)
		if !strings.HasSuffix(text, wantMarker) {
			t.Errorf("truncated text does not end with %q", wantMarker)
		}
		if !strings.Contains(strings.Join(gh.RequestLog(), "\n"), "/git/blobs/") {
			t.Errorf("a 5 MB file should be read through the blob API, requests:\n%s", strings.Join(gh.RequestLog(), "\n"))
		}
	})
}
