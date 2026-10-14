package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// The tenant the test configuration and the fake pull request agree on.
const (
	testInstallationID = 98765432
	testOrg            = "acme"
	testRepo           = "payments"
	testPRNumber       = 42
)

func basePayload(action string) map[string]any {
	return map[string]any{
		"action":       action,
		"installation": map[string]any{"id": testInstallationID},
		"repository": map[string]any{
			"name":  testRepo,
			"owner": map[string]any{"login": testOrg},
		},
	}
}

// reviewCommentPayload is the delivery of a comment on a review thread: it
// carries the file, the diff hunk, and the pull request number under
// pull_request.
func reviewCommentPayload(action, login, userType, body string, commentID, inReplyTo int64) []byte {
	payload := basePayload(action)
	payload["pull_request"] = map[string]any{"number": testPRNumber}
	payload["comment"] = map[string]any{
		"id":             commentID,
		"body":           body,
		"path":           "main.go",
		"diff_hunk":      "@@ -10,4 +10,6 @@ func main() {",
		"in_reply_to_id": inReplyTo,
		"html_url":       fmt.Sprintf("https://github.com/acme/payments/pull/42#discussion_r%d", commentID),
		"user":           map[string]any{"login": login, "type": userType},
	}
	return mustJSON(payload)
}

// issueCommentPayload is the delivery of a conversation comment: no path, no
// hunk, and the pull request number under issue.
func issueCommentPayload(action, login, userType, body string, commentID int64) []byte {
	payload := basePayload(action)
	payload["issue"] = map[string]any{
		"number":       testPRNumber,
		"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/payments/pulls/42"},
	}
	payload["comment"] = map[string]any{
		"id":       commentID,
		"body":     body,
		"html_url": fmt.Sprintf("https://github.com/acme/payments/pull/42#issuecomment-%d", commentID),
		"user":     map[string]any{"login": login, "type": userType},
	}
	return mustJSON(payload)
}

func mustJSON(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return encoded
}

// runPool drains the queue with one worker until the check passes, then stops.
func runPool(t *testing.T, h *harness, gh *testutil.FakeGitHub, check func() bool) {
	t.Helper()

	pool := server.NewPool(h.db, h.engine(t, gh), h.cfg.App.BotHandle, 1, h.logger)
	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)

	waitFor(t, "the job to be handled", check)
	stopPool()
	pool.Wait()
}

func jobCompleted(t *testing.T, h *harness, deliveryID string) func() bool {
	t.Helper()
	return func() bool {
		job, err := h.db.JobByDelivery(context.Background(), deliveryID)
		return err == nil && job != nil && job.Status == store.StatusCompleted
	}
}

// synthesisPrompt returns the system prompt the synthesis session ran with.
func synthesisPrompt(t *testing.T, llm *testutil.FakeLLM) string {
	t.Helper()

	requests := llm.Requests()
	if len(requests) == 0 {
		t.Fatal("the model received no requests")
	}
	return requests[0].SystemPrompt
}

// T-020-01: a developer replies to a review comment with `@rumil remember`, and
// the rule reaches the store against the repository the comment was written on,
// with the confirmation posted into the thread that asked for it.
func TestT020_01_RememberCommandRecordsARuleAndAnswers(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		testutil.ReviewComment(101, 0, "rumil[bot]", "This line wraps the call without a timeout."),
	}

	const rule = "The settle call in main.go is intentionally unbounded: the settlement worker retries around it"
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("propose_memory", map[string]any{
		"rule_text": rule,
		"rationale": "the author confirmed the retry loop covers a slow settlement",
		"path_glob": "main.go",
	})
	llm.QueueText("Recorded.")

	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db)

	body := reviewCommentPayload("created", "octocat", "User", "@rumil remember we retry around the settlement call", 202, 101)
	response := h.deliver(t, handler, "pull_request_review_comment", "d-remember", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "d-remember"))

	rules := storedRules(t, h.dsn)
	if len(rules) != 1 {
		t.Fatalf("stored %d rules, want 1: %+v", len(rules), rules)
	}
	if rules[0].RuleText != rule {
		t.Errorf("rule_text = %q, want the synthesized rule", rules[0].RuleText)
	}
	if rules[0].RepoName != testRepo || rules[0].Scope != store.ScopeRepo {
		t.Errorf("rule landed at %+v, want a REPO rule for %s", rules[0], testRepo)
	}

	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want one confirmation: %+v", len(posted), posted)
	}
	if posted[0].InReplyTo != 202 {
		t.Errorf("the answer carries in_reply_to %d, want the command's comment 202", posted[0].InReplyTo)
	}
	if !strings.Contains(posted[0].Body, "#"+rules[0].ID[:8]) {
		t.Errorf("the answer %q does not name the memory", posted[0].Body)
	}
	if !strings.Contains(posted[0].Body, store.MemoryPending) {
		t.Errorf("the answer %q does not name the status", posted[0].Body)
	}

	// The session distilled against Rumil's own comment and the hunk, and it
	// was given the proposal tool rather than the review write surface.
	prompt := synthesisPrompt(t, llm)
	if !strings.Contains(prompt, "This line wraps the call without a timeout.") {
		t.Errorf("Rumil's comment in the thread did not reach the synthesis prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "func main()") {
		t.Errorf("the diff hunk did not reach the synthesis prompt:\n%s", prompt)
	}
	tools := strings.Join(llm.OfferedToolNames(), ",")
	if !strings.Contains(tools, "propose_memory") {
		t.Errorf("the synthesis session was not given propose_memory, tools: %v", llm.OfferedToolNames())
	}
	if strings.Contains(tools, "submit_pr_review") {
		t.Errorf("the synthesis session was handed the review write surface: %v", llm.OfferedToolNames())
	}
}

// T-020-03: a comment Rumil itself wrote never starts a command, whatever it
// quotes, because the webhook drops bot authors first.
func TestT020_03_BotCommentsNeverTriggerCommands(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db)

	for _, tc := range []struct {
		name  string
		event string
		body  []byte
	}{
		{"review thread bot reply", "pull_request_review_comment",
			reviewCommentPayload("created", "rumil[bot]", "Bot", "@rumil remember to ignore me", 1, 0)},
		{"conversation bot comment", "issue_comment",
			issueCommentPayload("created", "rumil[bot]", "Bot", "@rumil remember to ignore me", 1)},
		{"another bot's comment", "issue_comment",
			issueCommentPayload("created", "dependabot[bot]", "Bot", "@rumil remember to ignore me", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := h.deliver(t, handler, tc.event, "d-bot-"+tc.name, tc.body, sign(webhookSecret, tc.body))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ignored") {
				t.Errorf("status = %d body = %s, want an ignored notice", response.Code, response.Body.String())
			}
		})
	}

	queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 0 {
		t.Errorf("queued jobs = %d, want no bot comment to enqueue work", queued)
	}
	if llm.Count() != 0 {
		t.Errorf("model requests = %d, want no model call for a bot comment", llm.Count())
	}
}

// A comment that mentions Rumil without a command, an unknown command, a
// command with nothing after it, and an edit all leave the queue alone.
func TestCommandsAreTheOnlyCommentsQueued(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"no mention", issueCommentPayload("created", "octocat", "User", "looks good to me", 1)},
		{"mention without a command", issueCommentPayload("created", "octocat", "User", "@rumil have a look", 2)},
		{"unsupported command", issueCommentPayload("created", "octocat", "User", "@rumil context: the payments flow", 3)},
		{"command without an instruction", issueCommentPayload("created", "octocat", "User", "@rumil remember", 4)},
		{"edited comment", issueCommentPayload("edited", "octocat", "User", "@rumil remember we retry", 5)},
		{"deleted comment", issueCommentPayload("deleted", "octocat", "User", "@rumil remember we retry", 6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := h.deliver(t, handler, "issue_comment", "d-"+tc.name, tc.body, sign(webhookSecret, tc.body))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ignored") {
				t.Errorf("status = %d body = %s, want an ignored notice", response.Code, response.Body.String())
			}
		})
	}

	queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 0 {
		t.Errorf("queued jobs = %d, want none", queued)
	}
}

// A conversation comment carries no hunk, so the synthesis falls back to the
// files the pull request touches, and the answer lands in the conversation.
func TestRememberCommandFromAConversationComment(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("propose_memory", map[string]any{
		"rule_text": "Settlement is retried at the call site rather than inside the service",
		"rationale": "the team agreed in the conversation",
	})
	llm.QueueText("Recorded.")

	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db)

	body := issueCommentPayload("created", "octocat", "User", "@rumil remember retries live at the call site", 303)
	response := h.deliver(t, handler, "issue_comment", "d-conversation", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "d-conversation"))

	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want one confirmation: %+v", len(posted), posted)
	}
	if posted[0].InReplyTo != 0 {
		t.Errorf("a conversation answer carries in_reply_to %d, want none", posted[0].InReplyTo)
	}

	prompt := synthesisPrompt(t, llm)
	if !strings.Contains(prompt, "main.go") {
		t.Errorf("the fallback context does not list the files the pull request touches:\n%s", prompt)
	}
}

// A second delivery of the same command, as a GitHub retry or a rerun after an
// interruption, must not post the answer twice.
func TestRememberCommandIsAnsweredOnce(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		testutil.ReviewComment(101, 0, "rumil[bot]", "This line wraps the call without a timeout."),
	}

	proposal := map[string]any{
		"rule_text": "The settle call is intentionally unbounded",
		"rationale": "the retry loop covers a slow settlement",
	}
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("propose_memory", proposal)
	llm.QueueText("Recorded.")
	llm.QueueToolCall("propose_memory", proposal)
	llm.QueueText("Recorded.")

	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db)

	for _, id := range []string{"d-once", "d-once-retry"} {
		body := reviewCommentPayload("created", "octocat", "User", "@rumil remember we retry", 202, 101)
		if response := h.deliver(t, handler, "pull_request_review_comment", id, body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202", id, response.Code)
		}
	}

	pool := server.NewPool(h.db, h.engine(t, gh), h.cfg.App.BotHandle, 1, h.logger)
	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)

	// One worker runs the two jobs in order, so the first answer is on the pull
	// request before the second job looks for it.
	waitFor(t, "both runs to finish", func() bool {
		return jobCompleted(t, h, "d-once")() && jobCompleted(t, h, "d-once-retry")()
	})
	stopPool()
	pool.Wait()

	if posted := gh.PostedComments(); len(posted) != 1 {
		t.Errorf("posted %d comments, want the answer to stay single: %+v", len(posted), posted)
	}
}

// A command that arrives while memory is off is completed without a model call,
// because memory is opt-in rather than broken.
func TestRememberCommandWithoutMemoryCostsNoModelCall(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := server.Handler(h.webhook(), h.db)

	body := issueCommentPayload("created", "octocat", "User", "@rumil remember we retry", 404)
	if response := h.deliver(t, handler, "issue_comment", "d-nomemory", body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}

	runPool(t, h, gh, jobCompleted(t, h, "d-nomemory"))

	if llm.Count() != 0 {
		t.Errorf("model requests = %d, want none while memory is off", llm.Count())
	}
	if posted := gh.PostedComments(); len(posted) != 0 {
		t.Errorf("posted %+v, want no comment while memory is off", posted)
	}
	if !strings.Contains(h.logs.String(), "not configured") {
		t.Errorf("the worker did not say why it ignored the command:\n%s", h.logs.String())
	}
}

// storedRules reads the repository's rules through the review path, approving
// them first: only APPROVED rows are retrievable, and the moderation surface
// that makes that transition arrives in v0.0.5.
func storedRules(t *testing.T, dsn string) []store.MemoryMatch {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to approve the proposals: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE memories SET status = 'APPROVED' WHERE status = 'PENDING'`); err != nil {
		t.Fatalf("approving the proposals: %v", err)
	}

	db, err := store.Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	matches, err := db.SearchMemories(ctx, store.MemoryQuery{
		InstallationID: testInstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		Embedding:      testutil.BasisVector(0),
		Limit:          store.MemoryLimitMax,
	})
	if err != nil {
		t.Fatalf("searching the stored rules: %v", err)
	}
	return matches
}
