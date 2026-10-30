package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/reviewer"
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

// testAssociation is the standing the fixtures' commenters have. A private
// repository accepts a command from anybody who can see it, which is what the
// operator of a self-hosted App usually has; the public-repository guard has
// tests of its own.
const testAssociation = "MEMBER"

func basePayload(action string) map[string]any {
	return map[string]any{
		"action":       action,
		"installation": map[string]any{"id": testInstallationID},
		"repository": map[string]any{
			"name":    testRepo,
			"owner":   map[string]any{"login": testOrg},
			"private": true,
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
		"id":                 commentID,
		"body":               body,
		"path":               "main.go",
		"diff_hunk":          "@@ -10,4 +10,6 @@ func main() {",
		"in_reply_to_id":     inReplyTo,
		"author_association": testAssociation,
		"html_url":           fmt.Sprintf("https://github.com/acme/payments/pull/42#discussion_r%d", commentID),
		"user":               map[string]any{"login": login, "type": userType},
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
		"id":                 commentID,
		"body":               body,
		"author_association": testAssociation,
		"html_url":           fmt.Sprintf("https://github.com/acme/payments/pull/42#issuecomment-%d", commentID),
		"user":               map[string]any{"login": login, "type": userType},
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
	handler := server.Handler(h.webhook(), h.db, nil)

	body := reviewCommentPayload("created", "octocat", "User", "@rumil remember we retry around the settlement call", 202, 101)
	response := h.deliver(t, handler, "pull_request_review_comment", "d-remember", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "d-remember"))

	rules := storedMemories(t, h.dsn)
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

// T-040-01 (first half): `--org` makes the rule the organization's. The
// synthesis session is told what was asked for, the row keeps the repository
// the command was written on as its origin, and the confirmation says what the
// rule reaches.
func TestT040_01_OrgScopedCommandRecordsAnOrganizationRule(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		testutil.ReviewComment(101, 0, "rumil[bot]", "This package logs with logrus while the rest of the organization moved on."),
	}

	const rule = "All Go services in the organization log with log/slog rather than logrus"
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("propose_memory", map[string]any{
		"rule_text": rule,
		"rationale": "the platform team agreed on one logger for every service",
		"scope":     store.ScopeOrg,
	})
	llm.QueueText("Recorded.")

	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db, nil)

	body := reviewCommentPayload("created", "octocat", "User",
		"@rumil remember --org all Go services must use log/slog instead of logrus", 202, 101)
	if response := h.deliver(t, handler, "pull_request_review_comment", "d-org", body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "d-org"))

	rules := storedMemories(t, h.dsn)
	if len(rules) != 1 {
		t.Fatalf("stored %d rules, want 1: %+v", len(rules), rules)
	}
	if rules[0].Scope != store.ScopeOrg {
		t.Errorf("scope = %s, want %s", rules[0].Scope, store.ScopeOrg)
	}
	if rules[0].OrgName != testOrg {
		t.Errorf("org_name = %q, want %q", rules[0].OrgName, testOrg)
	}
	// The origin repository stays on the row: an organization rule is still
	// recorded somewhere, and that is what an audit reads.
	if rules[0].RepoName != testRepo {
		t.Errorf("repo_name = %q, want the origin repository %q kept on the row", rules[0].RepoName, testRepo)
	}
	if rules[0].Status != store.MemoryPending {
		t.Errorf("status = %s, want %s while the gate is on", rules[0].Status, store.MemoryPending)
	}

	prompt := synthesisPrompt(t, llm)
	if !strings.Contains(prompt, "Requested scope: ORG, a rule for every repository in "+testOrg) {
		t.Errorf("the synthesis prompt does not name the requested scope:\n%s", prompt)
	}
	if !strings.Contains(prompt, "instead of logrus") {
		t.Errorf("the person's own words did not reach the synthesis prompt:\n%s", prompt)
	}

	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want one confirmation: %+v", len(posted), posted)
	}
	if !strings.Contains(posted[0].Body, "scope: ORG") {
		t.Errorf("the confirmation %q does not say what the rule reaches", posted[0].Body)
	}
}

// T-030-01: with the gate on, a rule a command records waits, and the next
// review of the same pull request does not retrieve it.
func TestT030_01_PendingRuleStaysOutOfTheNextReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Comments = []testutil.CommentEntry{
		testutil.ReviewComment(101, 0, "rumil[bot]", "This line wraps the call without a timeout."),
	}
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("propose_memory", map[string]any{
		"rule_text": "Clock skew up to 60 seconds is intentional in the JWT validator",
		"rationale": "the author confirmed it in review",
	})
	llm.QueueText("Recorded.")
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db, nil)

	body := reviewCommentPayload("created", "octocat", "User", "@rumil remember we allow 60s of clock skew", 202, 101)
	if response := h.deliver(t, handler, "pull_request_review_comment", "d-gate", body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}
	runPool(t, h, gh, jobCompleted(t, h, "d-gate"))

	rules := storedMemories(t, h.dsn)
	if len(rules) != 1 || rules[0].Status != store.MemoryPending {
		t.Fatalf("stored %+v, want one %s rule", rules, store.MemoryPending)
	}

	// The same repository now gets reviewed. The rule is on record, and the
	// prompt does not carry it.
	if _, err := h.engine(t, gh).Review(context.Background(), reviewer.Request{
		Owner: testOrg, Repo: testRepo, Number: testPRNumber,
	}); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	prompt := ""
	for _, request := range llm.Requests() {
		if strings.Contains(request.SystemPrompt, "Principal Code Reviewer") {
			prompt = request.SystemPrompt
			break
		}
	}
	if prompt == "" {
		t.Fatal("the review session sent no system prompt")
	}
	if strings.Contains(prompt, "Clock skew") {
		t.Errorf("an unapproved rule reached the review prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "<project_memories>\n") {
		t.Errorf("the prompt carries an empty or unapproved memory block:\n%s", prompt)
	}

	// The rule is out of the prompt because retrieval ran and left it out, not
	// because retrieval did not run: its query was embedded, and the row it
	// would have matched was on record while that happened.
	if len(llm.EmbeddedTexts()) == 0 {
		t.Error("the review embedded no retrieval query: nothing looked for the rule")
	}
}

// T-030-05: an operator who turned the gate off gets the rule straight away,
// and one who listed an association gets it for the people who hold that
// standing and not for anyone else.
func TestT030_05_AcceptAllAndListedAssociationsSkipTheGate(t *testing.T) {
	cases := []struct {
		name       string
		moderation string
		login      string
		want       string
	}{
		{
			name:       "accept_all approves everybody",
			moderation: "moderation:\n  accept_all: true\n",
			login:      "octocat",
			want:       store.MemoryApproved,
		},
		{
			name:       "a listed association approves its holder",
			moderation: "moderation:\n  auto_approve_associations: [\"MEMBER\"]\n",
			login:      "octocat",
			want:       store.MemoryApproved,
		},
		{
			name:       "an association nobody listed waits",
			moderation: "moderation:\n  auto_approve_associations: [\"OWNER\"]\n",
			login:      "octocat",
			want:       store.MemoryPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			llm := testutil.NewFakeLLM(t)
			llm.QueueToolCall("propose_memory", map[string]any{
				"rule_text": "Clock skew up to 60 seconds is intentional",
				"rationale": "the author confirmed it in review",
			})
			llm.QueueText("Recorded.")

			h := newHarness(t, gh, llm, harnessOpts{embedding: true, moderation: tc.moderation})
			handler := server.Handler(h.webhook(), h.db, nil)

			// testAssociation is MEMBER, and the fixture's repository is
			// private, so the command is accepted either way.
			body := reviewCommentPayload("created", tc.login, "User", "@rumil remember we allow clock skew", 303, 0)
			if response := h.deliver(t, handler, "pull_request_review_comment", "d-gate-"+tc.name, body, sign(webhookSecret, body)); response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body: %s)", response.Code, response.Body.String())
			}
			runPool(t, h, gh, jobCompleted(t, h, "d-gate-"+tc.name))

			rules := storedMemories(t, h.dsn)
			if len(rules) != 1 {
				t.Fatalf("stored %d rules, want 1", len(rules))
			}
			if rules[0].Status != tc.want {
				t.Errorf("status = %s, want %s", rules[0].Status, tc.want)
			}
		})
	}
}

// T-030-06 (second half): a commenter with no standing in a public repository
// cannot record a rule, and their command costs the operator nothing — no row,
// no model call, no embedding.
func TestT030_06_UntrustedCommenterIsDroppedBeforeAnyWork(t *testing.T) {
	cases := []struct {
		name        string
		private     bool
		association string
		wantStatus  int
		wantJobs    int
	}{
		{"a stranger on a public repository", false, "NONE", http.StatusOK, 0},
		{"a first-time contributor on a public repository", false, "FIRST_TIME_CONTRIBUTOR", http.StatusOK, 0},
		{"a stranger on a private repository", true, "NONE", http.StatusAccepted, 1},
		{"a member of a public repository", false, "MEMBER", http.StatusAccepted, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			llm := testutil.NewFakeLLM(t)
			llm.QueueToolCall("propose_memory", map[string]any{
				"rule_text": "Clock skew up to 60 seconds is intentional",
				"rationale": "the author confirmed it in review",
			})
			llm.QueueText("Recorded.")

			h := newHarness(t, gh, llm, harnessOpts{embedding: true})
			handler := server.Handler(h.webhook(), h.db, nil)

			body := rawCommentPayload(tc.private, tc.association, "@rumil remember we allow clock skew")
			response := h.deliver(t, handler, "pull_request_review_comment", "d-untrusted", body, sign(webhookSecret, body))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", response.Code, tc.wantStatus, response.Body.String())
			}

			if tc.wantStatus == http.StatusOK {
				if !bytes.Contains(response.Body.Bytes(), []byte("ignored")) {
					t.Errorf("body = %s, want an ignored notice", response.Body.String())
				}
				runPool(t, h, gh, func() bool { return true })
				if calls := llm.Count(); calls != 0 {
					t.Errorf("the model was called %d times for a dropped command", calls)
				}
				if embeddings := llm.EmbeddedTexts(); len(embeddings) != 0 {
					t.Errorf("a dropped command embedded %v", embeddings)
				}
				if rules := storedMemories(t, h.dsn); len(rules) != 0 {
					t.Errorf("a dropped command stored %+v", rules)
				}
			}

			queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
			if err != nil {
				t.Fatalf("CountJobs returned error: %v", err)
			}
			if queued != int(tc.wantJobs) {
				t.Errorf("queued jobs = %d, want %d", queued, tc.wantJobs)
			}
		})
	}
}

// rawCommentPayload is a comment delivery whose repository visibility and
// commenter standing the test chooses.
func rawCommentPayload(private bool, association, body string) []byte {
	payload := basePayload("created")
	payload["repository"] = map[string]any{
		"name":    testRepo,
		"owner":   map[string]any{"login": testOrg},
		"private": private,
	}
	payload["pull_request"] = map[string]any{"number": testPRNumber}
	payload["comment"] = map[string]any{
		"id":                 404,
		"body":               body,
		"path":               "main.go",
		"diff_hunk":          "@@ -10,4 +10,6 @@ func main() {",
		"author_association": association,
		"user":               map[string]any{"login": "stranger", "type": "User"},
	}
	return mustJSON(payload)
}

// T-020-03: a comment Rumil itself wrote never starts a command, whatever it
// quotes, because the webhook drops bot authors first.
func TestT020_03_BotCommentsNeverTriggerCommands(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := server.Handler(h.webhook(), h.db, nil)

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
	handler := server.Handler(h.webhook(), h.db, nil)

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
	handler := server.Handler(h.webhook(), h.db, nil)

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
	handler := server.Handler(h.webhook(), h.db, nil)

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
	handler := server.Handler(h.webhook(), h.db, nil)

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

// A review can be asked for in the conversation. It is the surface that works
// when the App cannot be requested as a reviewer, and the reaction on the
// comment is how the person knows the request was taken before the review
// appears.
func TestAReviewCommandQueuesAndAcknowledges(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := issueCommentPayload("created", "octocat", "User", "@rumil review", 9001)
	handler := server.Handler(h.webhook(), h.db, nil)

	response := h.deliver(t, handler, "issue_comment", "delivery-review-command", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "delivery-review-command"))

	if reviews := gh.SubmittedReviews(); len(reviews) == 0 {
		t.Error("the command produced no review")
	}
	reactions := gh.AddedReactions()
	if len(reactions) != 1 {
		t.Fatalf("recorded %d reactions, want one acknowledgement", len(reactions))
	}
	if reactions[0].Target != "issue_comment" || reactions[0].ID != 9001 {
		t.Errorf("reaction = %+v, want it on the comment that carried the command", reactions[0])
	}
	if reactions[0].Content != "+1" {
		t.Errorf("reaction content = %q, want %q", reactions[0].Content, "+1")
	}
}

// The words after the verb are the focus: they reach the task message, so the
// reviewer spends its effort where the person asked.
func TestAReviewCommandCarriesItsFocusIntoThePrompt(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := issueCommentPayload("created", "octocat", "User",
		"@rumil review. Focus on migration consistency and deployment instructions", 9002)
	handler := server.Handler(h.webhook(), h.db, nil)

	response := h.deliver(t, handler, "issue_comment", "delivery-review-focus", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "delivery-review-focus"))

	requests := llm.Requests()
	if len(requests) == 0 {
		t.Fatal("the model received no requests")
	}
	if !strings.Contains(requests[0].UserPrompt, "<focus>\nFocus on migration consistency and deployment instructions\n</focus>") {
		t.Errorf("the focus did not reach the task message:\n%s", requests[0].UserPrompt)
	}
}

// A review request has no comment to answer, so its acknowledgement goes on the
// pull request.
func TestAReviewRequestIsAcknowledged(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	handler := server.Handler(h.webhook(), h.db, nil)

	response := h.deliver(t, handler, "pull_request", "delivery-ack", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", response.Code, response.Body.String())
	}

	runPool(t, h, gh, jobCompleted(t, h, "delivery-ack"))

	reactions := gh.AddedReactions()
	if len(reactions) != 1 {
		t.Fatalf("recorded %d reactions, want one acknowledgement", len(reactions))
	}
	if reactions[0].Target != "pull_request" {
		t.Errorf("reaction = %+v, want it on the pull request", reactions[0])
	}
}

// A reaction that cannot be posted is a warning rather than a failed job: the
// review still runs and still lands.
func TestAFailedReactionDoesNotFailTheReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ReactionStatus = http.StatusForbidden
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	handler := server.Handler(h.webhook(), h.db, nil)

	h.deliver(t, handler, "pull_request", "delivery-ack-failed", body, sign(webhookSecret, body))
	runPool(t, h, gh, jobCompleted(t, h, "delivery-ack-failed"))

	if reviews := gh.SubmittedReviews(); len(reviews) == 0 {
		t.Error("a refused reaction stopped the review")
	}
	if !strings.Contains(h.logs.String(), "acknowledging the review request failed") {
		t.Errorf("logs do not record the refused reaction:\n%s", h.logs.String())
	}
}

// A review command spends the operator's model calls whether or not it records
// anything, so it is guarded by the same standing as a remembered rule.
func TestAReviewCommandNeedsStandingOnAPublicRepository(t *testing.T) {
	cases := []struct {
		name        string
		private     bool
		association string
		wantStatus  int
	}{
		{"a stranger on a public repository", false, "NONE", http.StatusOK},
		{"a member of a public repository", false, "MEMBER", http.StatusAccepted},
		{"a stranger on a private repository", true, "NONE", http.StatusAccepted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			llm := testutil.NewFakeLLM(t)
			h := newHarness(t, gh, llm, harnessOpts{})
			handler := server.Handler(h.webhook(), h.db, nil)

			body := rawCommentPayload(tc.private, tc.association, "@rumil review")
			response := h.deliver(t, handler, "pull_request_review_comment", "d-review-guard", body, sign(webhookSecret, body))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", response.Code, tc.wantStatus, response.Body.String())
			}

			queued, err := h.db.CountJobs(context.Background(), store.StatusQueued)
			if err != nil {
				t.Fatalf("CountJobs returned error: %v", err)
			}
			want := 0
			if tc.wantStatus == http.StatusAccepted {
				want = 1
			}
			if queued != want {
				t.Errorf("queued jobs = %d, want %d", queued, want)
			}
		})
	}
}

// storedMemories reads the rows as they are, with the status the gate gave
// them, which is what a test of the approval gate asserts on.
func storedMemories(t *testing.T, dsn string) []store.Memory {
	t.Helper()

	ctx := context.Background()
	db, err := store.Open(ctx, dsn, true, testutil.EmbeddingDimensions)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	defer db.Close()

	memories, err := db.ListMemories(ctx, store.MemoryFilter{InstallationID: testInstallationID})
	if err != nil {
		t.Fatalf("listing the stored rules: %v", err)
	}
	return memories
}
