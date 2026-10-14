package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const webhookSecret = "test-webhook-secret"

// testConfigTemplate mirrors rumil.example.yaml with the fake endpoints
// injected, so the worker can run a whole review against the doubles.
const testConfigTemplate = `
app:
  bot_handle: "@rumil"
  max_agent_turns: 6
  log_level: "info"
github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: %q
  webhook_secret: %q
worker:
  concurrency: %d
server:
  listen: "127.0.0.1:0"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-model"
      api_key: "chat-key"
    - id: "embed"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-embedding"
      api_key: "embed-key"
      dimensions: 1536
%s
database:
  dsn: %q
  auto_migrate: true
`

type harness struct {
	db     *store.DB
	dsn    string
	cfg    *config.Config
	logs   *bytes.Buffer
	logger *slog.Logger
}

// newHarness opens the test database and loads a configuration that points at
// the fake GitHub and model endpoints.
func newHarness(t *testing.T, gh *testutil.FakeGitHub, llm *testutil.FakeLLM, opts harnessOpts) *harness {
	t.Helper()

	dsn := testutil.TestDatabaseURL(t, "rumil_server_test")
	if opts.concurrency == 0 {
		opts.concurrency = 1
	}

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}

	models := ""
	if opts.embedding {
		models = "  default_embedding: \"embed\"\n"
	}

	doc := fmt.Sprintf(testConfigTemplate, keyPath, webhookSecret, opts.concurrency,
		llm.URL()+"/v1", llm.URL()+"/v1", models, dsn)
	cfg, err := config.Parse([]byte(doc), "server_test.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v", err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	truncateJobs(t, dsn)

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return &harness{db: db, dsn: dsn, cfg: cfg, logs: logs, logger: logger}
}

// truncateJobs empties the queue and the memory table through its own
// connection, so a test does not need a handle on the store's pool.
func truncateJobs(t *testing.T, dsn string) {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to truncate webhook_jobs: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `TRUNCATE webhook_jobs, memories`); err != nil {
		t.Fatalf("truncating the test tables: %v", err)
	}
}

type harnessOpts struct {
	concurrency int
	// embedding adds models.default_embedding, which together with
	// database.dsn is what turns memory on.
	embedding bool
}

func (h *harness) webhook() *server.Webhook {
	return server.NewWebhook(h.cfg.GitHub.WebhookSecret, h.cfg.App.BotHandle, h.db, h.logger)
}

func (h *harness) engine(t *testing.T, gh *testutil.FakeGitHub) *reviewer.Engine {
	t.Helper()

	engine, err := reviewer.NewEngine(h.cfg, h.logger, gh.URL(), h.db)
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	return engine
}

// deliver posts a signed payload to the webhook and returns the response.
func (h *harness) deliver(t *testing.T, handler http.Handler, eventType, deliveryID string, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", eventType)
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	if signature != "" {
		request.Header.Set("X-Hub-Signature-256", signature)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// sign returns the X-Hub-Signature-256 value GitHub would send.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// reviewRequestedPayload is a pull_request/review_requested delivery aimed at
// the configured bot.
func reviewRequestedPayload(action, reviewerLogin, reviewerType string) []byte {
	payload := map[string]any{
		"action":       action,
		"installation": map[string]any{"id": 98765432},
		"repository": map[string]any{
			"name":  "payments",
			"owner": map[string]any{"login": "acme"},
		},
		"pull_request": map[string]any{"number": 42},
		"requested_reviewer": map[string]any{
			"login": reviewerLogin,
			"type":  reviewerType,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return encoded
}

// scriptReview queues the calls a minimal review needs and returns the fake
// model, so a test can assert what the worker ran.
func scriptReview(llm *testutil.FakeLLM) {
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks good.",
	})
	llm.QueueText("Review submitted as COMMENT.")
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// T-010-01: a signed review_requested delivery is accepted quickly, queued,
// picked up by a worker, reviewed, and marked COMPLETED.
func TestT010_01_ReviewRequestedIsQueuedAndReviewed(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	handler := server.Handler(h.webhook(), h.db)

	started := time.Now()
	response := h.deliver(t, handler, "pull_request", "delivery-1", body, sign(webhookSecret, body))
	elapsed := time.Since(started)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", response.Code, response.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("the handler took %s, want under 200ms", elapsed)
	}

	job, err := h.db.JobByDelivery(context.Background(), "delivery-1")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if job == nil {
		t.Fatal("no job was enqueued")
	}
	if job.Status != store.StatusQueued {
		t.Errorf("status = %s, want %s", job.Status, store.StatusQueued)
	}
	if job.PRNumber != 42 || job.OrgName != "acme" || job.RepoName != "payments" {
		t.Errorf("job coordinates = %+v, want acme/payments#42", job)
	}

	pool := server.NewPool(h.db, h.engine(t, gh), "@rumil", 1, h.logger)
	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)

	waitFor(t, "the job to complete", func() bool {
		current, err := h.db.JobByDelivery(context.Background(), "delivery-1")
		return err == nil && current != nil && current.Status == store.StatusCompleted
	})

	stopPool()
	pool.Wait()

	if llm.Count() == 0 {
		t.Error("the worker never called the model")
	}
	if !bytes.Contains(h.logs.Bytes(), []byte("review finished")) {
		t.Errorf("logs do not record the finished review:\n%s", h.logs.String())
	}
}

// T-010-02: a delivery without a valid signature is rejected and writes
// nothing.
func TestT010_02_InvalidSignatureIsRejected(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := server.Handler(h.webhook(), h.db)

	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")

	cases := map[string]string{
		"missing":   "",
		"wrong":     sign("not-the-secret", body),
		"malformed": "sha256=zzzz",
		"no prefix": hex.EncodeToString([]byte("nonsense")),
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			response := h.deliver(t, handler, "pull_request", "delivery-"+name, body, signature)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", response.Code)
			}
		})
	}

	queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 0 {
		t.Errorf("queued jobs = %d, want no writes for unauthenticated deliveries", queued)
	}
	if llm.Count() != 0 {
		t.Errorf("model requests = %d, want none", llm.Count())
	}
}

// T-010-03: GitHub retrying the same delivery does not queue the work twice.
func TestT010_03_DuplicateDeliveryIsAbsorbed(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	handler := server.Handler(h.webhook(), h.db)
	signature := sign(webhookSecret, body)

	first := h.deliver(t, handler, "pull_request", "delivery-1", body, signature)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.Code)
	}

	second := h.deliver(t, handler, "pull_request", "delivery-1", body, signature)
	if second.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200", second.Code)
	}
	if !bytes.Contains(second.Body.Bytes(), []byte("duplicate")) {
		t.Errorf("retry body = %s, want a duplicate notice", second.Body.String())
	}

	queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 1 {
		t.Errorf("queued jobs = %d, want the retry to add none", queued)
	}

	// A worker runs one review, not two.
	pool := server.NewPool(h.db, h.engine(t, gh), "@rumil", 1, h.logger)
	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)
	waitFor(t, "the job to complete", func() bool {
		current, err := h.db.JobByDelivery(context.Background(), "delivery-1")
		return err == nil && current != nil && current.Status == store.StatusCompleted
	})
	stopPool()
	pool.Wait()

	if reviews := len(llm.Requests()); reviews == 0 || reviews > 2 {
		t.Errorf("model requests = %d, want one review's worth", reviews)
	}
}

// T-010-04: only a review request aimed at Rumil queues work.
func TestT010_04_NoiseIsIgnored(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := server.Handler(h.webhook(), h.db)

	cases := []struct {
		name     string
		event    string
		body     []byte
		delivery string
	}{
		{"opened", "pull_request", reviewRequestedPayload("opened", "rumil[bot]", "Bot"), "d-opened"},
		{"synchronize", "pull_request", reviewRequestedPayload("synchronize", "rumil[bot]", "Bot"), "d-sync"},
		{"labeled", "pull_request", reviewRequestedPayload("labeled", "rumil[bot]", "Bot"), "d-labeled"},
		{"human reviewer", "pull_request", reviewRequestedPayload("review_requested", "octocat", "User"), "d-human"},
		{"someone else's bot", "pull_request", reviewRequestedPayload("review_requested", "dependabot[bot]", "Bot"), "d-other-bot"},
		{"plain issue comment", "issue_comment", []byte(`{"action":"created","issue":{"number":7},"repository":{"name":"payments","owner":{"login":"acme"}}}`), "d-issue"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := h.deliver(t, handler, tc.event, tc.delivery, tc.body, sign(webhookSecret, tc.body))
			if response.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 with an ignored notice", response.Code)
			}
			if !bytes.Contains(response.Body.Bytes(), []byte("ignored")) {
				t.Errorf("body = %s, want an ignored notice", response.Body.String())
			}
		})
	}

	queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 0 {
		t.Errorf("queued jobs = %d, want none of the noise to enqueue work", queued)
	}
}

// T-010-05: a shutdown stops accepting deliveries, and a job interrupted
// mid-review returns to the queue on the next start.
func TestT010_05_ShutdownStopsAcceptingAndRequeues(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := server.Handler(h.webhook(), h.db)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()

	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	url := "http://" + listener.Addr().String() + "/webhooks/github"

	post := func() int {
		request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		request.Header.Set("X-GitHub-Event", "pull_request")
		request.Header.Set("X-GitHub-Delivery", "delivery-1")
		request.Header.Set("X-Hub-Signature-256", sign(webhookSecret, body))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}

	if status := post(); status != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", status)
	}

	// Claim the job and leave it RUNNING, which is what a SIGKILL mid-review
	// looks like to the next process.
	job, err := h.db.ClaimJob(context.Background())
	if err != nil || job == nil {
		t.Fatalf("ClaimJob returned (%v, %v), want a job", job, err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	if status := post(); status != 0 {
		t.Errorf("delivery after shutdown returned %d, want the server to stop accepting", status)
	}

	moved, err := h.db.ResetRunningJobs(context.Background())
	if err != nil {
		t.Fatalf("ResetRunningJobs returned error: %v", err)
	}
	if moved != 1 {
		t.Errorf("requeued %d jobs, want the interrupted one", moved)
	}

	requeued, err := h.db.JobByDelivery(context.Background(), "delivery-1")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if requeued.Status != store.StatusQueued {
		t.Errorf("status = %s, want %s on the next start", requeued.Status, store.StatusQueued)
	}
}
