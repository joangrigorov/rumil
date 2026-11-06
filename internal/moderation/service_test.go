package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// The status filter is normalized in one place, which is where both the CLI and
// the HTTP surface pass through, so neither can hand the statement a value the
// enum will refuse.
func TestNormalizeStatusAcceptsTheThreeAndNothingElse(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"nothing asked for", "", ""},
		{"as the enum spells it", "PENDING", "PENDING"},
		{"as a moderator types it", "approved", "APPROVED"},
		{"with padding", "  Rejected  ", "REJECTED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeStatus(tc.status)
			if err != nil {
				t.Fatalf("normalizeStatus(%q) returned error: %v", tc.status, err)
			}
			if got != tc.want {
				t.Errorf("normalizeStatus(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}

	for _, status := range []string{"approve", "pending2", "any", "PENDING,APPROVED"} {
		t.Run("refused: "+status, func(t *testing.T) {
			got, err := normalizeStatus(status)
			if !errors.Is(err, ErrUnknownStatus) {
				t.Fatalf("normalizeStatus(%q) returned %v, want %v", status, err, ErrUnknownStatus)
			}
			if got != "" {
				t.Errorf("a refused status came back as %q", got)
			}
		})
	}
}

// The authority a decision carries is normalized in the same one place the
// status and the scope are, so a surface cannot hand the statement a value the
// enum will refuse.
func TestNormalizeAuthorityAcceptsTheTwoAndNothingElse(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		want      string
	}{
		{"nothing asked for", "", ""},
		{"as the enum spells it", "CONTEXT", "CONTEXT"},
		{"as a moderator types it", "directive", "DIRECTIVE"},
		{"with padding", "  Context  ", "CONTEXT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeAuthority(tc.authority)
			if err != nil {
				t.Fatalf("normalizeAuthority(%q) returned error: %v", tc.authority, err)
			}
			if got != tc.want {
				t.Errorf("normalizeAuthority(%q) = %q, want %q", tc.authority, got, tc.want)
			}
		})
	}

	for _, authority := range []string{"absolute", "directives", "CONTEXT,DIRECTIVE", "repo"} {
		t.Run("refused: "+authority, func(t *testing.T) {
			got, err := normalizeAuthority(authority)
			if !errors.Is(err, ErrUnknownAuthority) {
				t.Fatalf("normalizeAuthority(%q) returned %v, want %v", authority, err, ErrUnknownAuthority)
			}
			if got != "" {
				t.Errorf("a refused authority came back as %q", got)
			}
		})
	}
}

type commentCall struct {
	installationID int64
	owner          string
	repo           string
	number         int
	shortID        string
	body           string
}

type fakeCommentEditor struct {
	calls  []commentCall
	edited int
	err    error
}

func (f *fakeCommentEditor) RewriteMemoryComments(_ context.Context, installationID int64, owner, repo string, number int, shortID, body string) (int, error) {
	f.calls = append(f.calls, commentCall{
		installationID: installationID,
		owner:          owner,
		repo:           repo,
		number:         number,
		shortID:        shortID,
		body:           body,
	})
	return f.edited, f.err
}

func testMemory() store.Memory {
	return store.Memory{
		ID:             "2f06cd14-1111-2222-3333-444444444444",
		InstallationID: 42,
		OrgName:        "acme",
		RepoName:       "payments",
		Scope:          store.ScopeRepo,
		Authority:      store.AuthorityContext,
		Status:         store.MemoryApproved,
		SourcePR:       1613,
	}
}

func TestSyncMemoryCommentRewritesTheSourcePullRequest(t *testing.T) {
	editor := &fakeCommentEditor{edited: 2}
	service := &Service{comments: editor, installationID: 42, logger: slog.New(slog.DiscardHandler)}

	service.syncMemoryComment(context.Background(), testMemory())

	if len(editor.calls) != 1 {
		t.Fatalf("editor called %d times, want 1", len(editor.calls))
	}
	call := editor.calls[0]
	if call.installationID != 42 || call.owner != "acme" || call.repo != "payments" || call.number != 1613 {
		t.Errorf("editor called with %+v, want the memory's installation and pull request", call)
	}
	if call.shortID != "2f06cd14" {
		t.Errorf("short id = %q, want 2f06cd14", call.shortID)
	}
	if !strings.Contains(call.body, "`#2f06cd14` approved") || !strings.Contains(call.body, "`status: APPROVED`") {
		t.Errorf("outcome body = %q, want the approved status and the short id", call.body)
	}
}

func TestSyncMemoryCommentDoesNotFailTheDecision(t *testing.T) {
	editor := &fakeCommentEditor{err: errors.New("github is down")}
	service := &Service{comments: editor, installationID: 42, logger: slog.New(slog.DiscardHandler)}

	// The decision is already stored, so a comment edit failure is logged and
	// swallowed rather than raised to the caller.
	service.syncMemoryComment(context.Background(), testMemory())

	if len(editor.calls) != 1 {
		t.Fatalf("editor called %d times, want 1", len(editor.calls))
	}
}

func TestSyncMemoryCommentSkipsWhatItCannotAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		editor CommentEditor
		memory store.Memory
	}{
		{"no editor", nil, testMemory()},
		{"no source pull request", &fakeCommentEditor{}, func() store.Memory {
			memory := testMemory()
			memory.SourcePR = 0
			return memory
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{comments: tc.editor, installationID: 42, logger: slog.New(slog.DiscardHandler)}
			service.syncMemoryComment(context.Background(), tc.memory)
			if editor, ok := tc.editor.(*fakeCommentEditor); ok && len(editor.calls) != 0 {
				t.Errorf("editor called %d times, want none", len(editor.calls))
			}
		})
	}
}

func TestDecisionsRewriteTheConfirmationComment(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_moderation_test")
	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	// The schema is shared between runs, so the repository and the rule text
	// are unique and a previous run's rules cannot look like duplicates.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	proposal := func(text string, axis int) store.Proposal {
		embedding := make([]float32, testutil.EmbeddingDimensions)
		embedding[axis] = 1
		return store.Proposal{
			InstallationID: 4242,
			OrgName:        "moderation-test",
			RepoName:       "repo-" + suffix,
			Scope:          store.ScopeRepo,
			RuleText:       text + " " + suffix,
			Rationale:      "recorded in a test",
			SourcePR:       1613,
			SourceComment:  "https://github.com/moderation-test/repo/pull/1613#discussion_r1",
			RequestedBy:    "octocat",
			Embedding:      embedding,
		}
	}

	t.Run("approval", func(t *testing.T) {
		pending, inserted, err := db.ProposeMemory(ctx, proposal("A rule that is approved", 0))
		if err != nil {
			t.Fatalf("ProposeMemory returned error: %v", err)
		}
		if !inserted {
			t.Fatal("the fixture was answered as a duplicate")
		}

		editor := &fakeCommentEditor{edited: 1}
		service := NewService(db, nil, pending.InstallationID, slog.New(slog.DiscardHandler), WithCommentEditor(editor))

		if _, err := service.Approve(ctx, pending.ShortID(), Decision{Moderator: "admin"}); err != nil {
			t.Fatalf("Approve returned error: %v", err)
		}
		if len(editor.calls) != 1 {
			t.Fatalf("editor called %d times, want 1", len(editor.calls))
		}
		call := editor.calls[0]
		if call.shortID != pending.ShortID() || call.number != 1613 {
			t.Errorf("editor called with %+v, want %s on PR 1613", call, pending.ShortID())
		}
		if !strings.Contains(call.body, "`status: APPROVED`") {
			t.Errorf("outcome body = %q, want APPROVED", call.body)
		}
	})

	t.Run("rejection", func(t *testing.T) {
		pending, inserted, err := db.ProposeMemory(ctx, proposal("A rule that is rejected", 1))
		if err != nil {
			t.Fatalf("ProposeMemory returned error: %v", err)
		}
		if !inserted {
			t.Fatal("the fixture was answered as a duplicate")
		}

		editor := &fakeCommentEditor{edited: 1}
		service := NewService(db, nil, pending.InstallationID, slog.New(slog.DiscardHandler), WithCommentEditor(editor))

		if _, err := service.Reject(ctx, pending.ShortID(), "admin"); err != nil {
			t.Fatalf("Reject returned error: %v", err)
		}
		if len(editor.calls) != 1 {
			t.Fatalf("editor called %d times, want 1", len(editor.calls))
		}
		if body := editor.calls[0].body; !strings.Contains(body, "`status: REJECTED`") {
			t.Errorf("outcome body = %q, want REJECTED", body)
		}
	})
}

func TestMemoryOutcomeBodyStatesTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory store.Memory
		want   []string
	}{
		{
			name:   "approved context",
			memory: testMemory(),
			want:   []string{"`#2f06cd14` approved", "`status: APPROVED`", "scope: REPO", "available to reviews"},
		},
		{
			name: "approved directive",
			memory: func() store.Memory {
				memory := testMemory()
				memory.Authority = store.AuthorityDirective
				return memory
			}(),
			want: []string{"`status: APPROVED`", "directive"},
		},
		{
			name: "rejected",
			memory: func() store.Memory {
				memory := testMemory()
				memory.Status = store.MemoryRejected
				return memory
			}(),
			want: []string{"`#2f06cd14` rejected", "`status: REJECTED`", "not used in reviews"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := memoryOutcomeBody(tc.memory)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("outcome body = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}
