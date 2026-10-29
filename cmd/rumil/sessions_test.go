package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// writeConfigWithDatabase renders a configuration the sessions commands can
// open: the queue lives in Postgres, and no model is called by this surface.
func writeConfigWithDatabase(t *testing.T, dsn string) string {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}

	doc := fmt.Sprintf(`
app:
  bot_handle: "@rumil"
  max_agent_turns: 5
github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: %q
database:
  dsn: %q
  auto_migrate: true
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "http://127.0.0.1:1/v1"
      model_name: "test-model"
      api_key: "test-key"
`, keyPath, dsn)

	configPath := filepath.Join(t.TempDir(), "rumil.yaml")
	if err := os.WriteFile(configPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the test config: %v", err)
	}
	return configPath
}

func openSessionsStore(t *testing.T, dsn string) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedSession(t *testing.T, db *store.DB, outcome store.ReviewOutcome) store.ReviewSession {
	t.Helper()

	ctx := context.Background()
	session, err := db.CreateReviewSession(ctx, store.NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating the session: %v", err)
	}
	if outcome.Status != store.ReviewRunning {
		if err := db.FinishReviewSession(ctx, session.ID, outcome); err != nil {
			t.Fatalf("finishing the session: %v", err)
		}
	}
	stored, err := db.ReviewSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	return stored
}

func TestSessionsContinueQueuesAnAttempt(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_cli_sessions_test")
	configPath := writeConfigWithDatabase(t, dsn)
	db := openSessionsStore(t, dsn)

	parent := seedSession(t, db, store.ReviewOutcome{
		Status:           store.ReviewFailed,
		Summary:          "I had started reading the diff.",
		Error:            "review session ended without submitting a review (turn limit 5 reached)",
		TurnLimitReached: true,
	})

	out, err := runRoot(t, "sessions", "continue", parent.ID, "--config", configPath, "--turns", "7", "--requested-by", "alice")
	if err != nil {
		t.Fatalf("sessions continue returned error: %v (output: %s)", err, out)
	}
	for _, want := range []string{parent.ID, "acme/payments#42", "7 turns", "alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to carry %q", out, want)
		}
	}

	job, err := db.JobByDelivery(context.Background(), continuation.DeliveryID(parent.ID))
	if err != nil {
		t.Fatalf("reading the queued job: %v", err)
	}
	if job == nil {
		t.Fatal("no continuation job was queued")
	}
	request, err := continuation.Parse(job.Payload)
	if err != nil {
		t.Fatalf("parsing the queued request: %v", err)
	}
	if request.SessionID != parent.ID || request.Turns != 7 || request.RequestedBy != "alice" {
		t.Errorf("queued request = %+v, want the parent, seven turns and alice", request)
	}
}

func TestSessionsContinueRefusesWhatCannotContinue(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_cli_sessions_test")
	configPath := writeConfigWithDatabase(t, dsn)
	db := openSessionsStore(t, dsn)

	turnLimited := seedSession(t, db, store.ReviewOutcome{
		Status:           store.ReviewFailed,
		Error:            "review session ended without submitting a review (turn limit 5 reached)",
		TurnLimitReached: true,
	})
	if _, err := runRoot(t, "sessions", "continue", turnLimited.ID, "--config", configPath); err != nil {
		t.Fatalf("the first continuation returned error: %v", err)
	}

	succeeded := seedSession(t, db, store.ReviewOutcome{
		Status:      store.ReviewSucceeded,
		ReviewEvent: "COMMENT",
	})
	running := seedSession(t, db, store.ReviewOutcome{Status: store.ReviewRunning})

	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "already queued", id: turnLimited.ID, want: "already has a continuation"},
		{name: "a submitted review", id: succeeded.ID, want: "already submitted a review"},
		{name: "still running", id: running.ID, want: "only a failed session"},
		{name: "no such session", id: "00000000-0000-0000-0000-000000000000", want: "no review session"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRoot(t, "sessions", "continue", tc.id, "--config", configPath)
			if err == nil {
				t.Fatalf("sessions continue succeeded, want a refusal (output: %s)", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A failure that is not the turn limit, such as a provider error, keeps the
// transcript that is worth reusing, so it can be continued too.
func TestSessionsContinueAcceptsAFailureWithAnotherCause(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_cli_sessions_test")
	configPath := writeConfigWithDatabase(t, dsn)
	db := openSessionsStore(t, dsn)

	parent := seedSession(t, db, store.ReviewOutcome{
		Status: store.ReviewFailed,
		Error:  "the GitHub App is not installed",
	})

	out, err := runRoot(t, "sessions", "continue", parent.ID, "--config", configPath)
	if err != nil {
		t.Fatalf("sessions continue returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, parent.ID) {
		t.Errorf("output = %q, want it to name %s", out, parent.ID)
	}
}
