package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/embedder"
	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const adminToken = "test-admin-token"

// textEmbedder is the embedding provider a moderation test edits rules with.
// The vector is a function of the text, so a rule approved against the edited
// wording is findable by that wording and not by the old one, which is exactly
// what T-030-03 asks about.
type textEmbedder struct {
	texts []string
}

func (e *textEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.texts = append(e.texts, text)
	return vectorFor(text), nil
}

// vectorFor is the vector every test uses for a piece of text: a basis vector
// chosen by the text, so two different texts never look alike.
func vectorFor(text string) []float32 {
	vector := make([]float32, config.EmbeddingDimensions)
	index := 0
	for _, r := range text {
		index = (index*31 + int(r)) % config.EmbeddingDimensions
	}
	vector[index] = 1
	return vector
}

// seedMemory records a pending rule and returns it.
func seedMemory(t *testing.T, h *harness, rule string) store.Memory {
	t.Helper()

	memory, _, err := h.db.ProposeMemory(context.Background(), store.Proposal{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		Scope:          store.ScopeRepo,
		RuleText:       rule,
		Rationale:      "the author confirmed it in review",
		SourcePR:       42,
		SourceComment:  "https://github.com/acme/payments/pull/42#discussion_r1",
		RequestedBy:    "octocat",
		Embedding:      vectorFor(rule),
	})
	if err != nil {
		t.Fatalf("seeding a rule: %v", err)
	}
	return memory
}

// moderationHandler mounts only the moderation surface, with the harness's
// database behind it.
func moderationHandler(t *testing.T, h *harness, embedder moderation.Embedder, token string) http.Handler {
	t.Helper()

	service := moderation.NewService(h.db, embedder, h.cfg.GitHub.InstallationID, h.logger)
	return server.Handler(h.webhook(), h.db, server.NewModerationUI(service, token, h.logger))
}

func moderatedRequest(t *testing.T, handler http.Handler, method, path, token string, form url.Values, accept string) *httptest.ResponseRecorder {
	t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, path, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// T-030-06 (first half): the moderation surface answers nobody without the
// configured token, and the comparison is what decides, not the caller's
// framing of it.
func TestT030_06_ModerationSurfaceNeedsTheToken(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	rule := seedMemory(t, h, "Clock skew up to 60 seconds is intentional")
	handler := moderationHandler(t, h, &textEmbedder{}, adminToken)

	cases := []struct {
		name   string
		token  string
		header string
		want   int
	}{
		{"no credential", "", "", http.StatusUnauthorized},
		{"a wrong token", "nope", "", http.StatusUnauthorized},
		{"a token that is a prefix", adminToken[:5], "", http.StatusUnauthorized},
		{"the token", adminToken, "", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := handler
			response := moderatedRequest(t, handler, http.MethodGet, "/ui/moderation", tc.token, nil, "text/html")
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", response.Code, tc.want, response.Body.String())
			}
			if tc.want == http.StatusOK && !strings.Contains(response.Body.String(), "Clock skew up to 60 seconds") {
				t.Errorf("the page does not list the rule:\n%s", response.Body.String())
			}
		})
	}

	// The API takes the same credential.
	approve := moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+rule.ID+"/approve", "", url.Values{}, "")
	if approve.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous approval returned %d, want 401", approve.Code)
	}
	if stored, err := h.db.MemoryByID(context.Background(), 98765432, rule.ID); err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	} else if stored.Status != store.MemoryPending {
		t.Errorf("an anonymous request changed the row to %s", stored.Status)
	}
}

// A surface with no token configured refuses everything rather than becoming an
// open admin route.
func TestModerationSurfaceWithoutATokenRefuses(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := moderationHandler(t, h, &textEmbedder{}, "")

	response := moderatedRequest(t, handler, http.MethodGet, "/ui/moderation", "anything", nil, "")
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while no token is configured", response.Code)
	}
}

// A browser cannot set an Authorization header of its own, so the token is also
// accepted as the password of a basic credential. The HTML page is usable in a
// browser without a login route, a cookie, or a line of JavaScript.
func TestModerationPageAcceptsBasicCredentials(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	seedMemory(t, h, "Every handler is a single-statement upsert")
	handler := moderationHandler(t, h, &textEmbedder{}, adminToken)

	request := httptest.NewRequest(http.MethodGet, "/ui/moderation", nil)
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("maintainer:"+adminToken)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Every handler is a single-statement upsert") {
		t.Errorf("the page does not list the rule:\n%s", recorder.Body.String())
	}
}

// T-030-02 and T-030-03 over HTTP: approving keeps the rule, records who
// decided, and re-embeds an edited wording. T-030-04: rejecting drops it from
// every read while the row stays.
func TestModerationApprovesAndRejects(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})

	proposed := "Clock skew up to 60 seconds is intentional"
	edited := "Clock skew up to 60 seconds is intentional in the JWT validator only"
	approvedRule := seedMemory(t, h, proposed)
	rejectedRule := seedMemory(t, h, "Settlements are retried three times")
	embedder := &textEmbedder{}
	handler := moderationHandler(t, h, embedder, adminToken)

	response := moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+approvedRule.ID+"/approve", adminToken,
		url.Values{"rule_text": {edited}, "path_glob": {"pkg/auth/**"}}, "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("approve returned %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Rule   string `json:"rule"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the approve result: %v", err)
	}
	if body.Status != store.MemoryApproved || body.Rule != edited {
		t.Errorf("approved %+v, want the edited rule kept", body)
	}
	if len(embedder.texts) != 1 || embedder.texts[0] != edited {
		t.Errorf("embedded %v, want exactly the edited text", embedder.texts)
	}

	stored, err := h.db.MemoryByID(context.Background(), 98765432, approvedRule.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if stored.Status != store.MemoryApproved || stored.ReviewedBy != "admin-token" {
		t.Errorf("row is %+v, want APPROVED and decided by the token holder", stored)
	}
	if stored.PathGlob != "pkg/auth/**" {
		t.Errorf("path_glob = %q, want the glob the moderator set", stored.PathGlob)
	}

	// The rule is found by the words that were approved, not by the words that
	// were proposed.
	newText, err := h.db.SearchMemories(context.Background(), store.MemoryQuery{
		InstallationID: 98765432, OrgName: "acme", RepoName: "payments", Embedding: vectorFor(edited),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	if len(newText) != 1 || newText[0].ID != approvedRule.ID {
		t.Errorf("the edited rule is not retrievable by its new wording: %+v", newText)
	}
	oldText, err := h.db.SearchMemories(context.Background(), store.MemoryQuery{
		InstallationID: 98765432, OrgName: "acme", RepoName: "payments", Embedding: vectorFor(proposed),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	for _, match := range oldText {
		if match.ID == approvedRule.ID && match.Similarity > 0.5 {
			t.Errorf("the old wording still matches the rule (%.3f): the edit was not re-embedded", match.Similarity)
		}
	}

	// Rejection is the other decision, and it hides the rule from searches.
	response = moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+rejectedRule.ID+"/reject", adminToken, url.Values{}, "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("reject returned %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	rejected, err := h.db.MemoryByID(context.Background(), 98765432, rejectedRule.ID)
	if err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	}
	if rejected.Status != store.MemoryRejected {
		t.Errorf("status = %s, want %s", rejected.Status, store.MemoryRejected)
	}
	matches, err := h.db.SearchMemories(context.Background(), store.MemoryQuery{
		InstallationID: 98765432, OrgName: "acme", RepoName: "payments", Embedding: vectorFor(rejected.RuleText),
	})
	if err != nil {
		t.Fatalf("SearchMemories returned error: %v", err)
	}
	for _, match := range matches {
		if match.ID == rejectedRule.ID {
			t.Errorf("a rejected rule is still retrievable: %+v", match)
		}
	}
}

// An edit without an embedding provider is refused rather than stored against a
// vector that no longer matches the words.
func TestModerationRefusesAnEditWithoutAnEmbedder(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	rule := seedMemory(t, h, "Clock skew up to 60 seconds is intentional")
	handler := moderationHandler(t, h, nil, adminToken)

	response := moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+rule.ID+"/approve", adminToken,
		url.Values{"rule_text": {"a rewritten rule"}}, "application/json")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", response.Code, response.Body.String())
	}
	if stored, err := h.db.MemoryByID(context.Background(), 98765432, rule.ID); err != nil {
		t.Fatalf("MemoryByID returned error: %v", err)
	} else if stored.Status != store.MemoryPending {
		t.Errorf("a refused edit changed the row to %s", stored.Status)
	}

	// Approving the wording as it was proposed needs no embedder, so the rule
	// is still usable when memory's providers are not configured.
	response = moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+rule.ID+"/approve", adminToken, url.Values{}, "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without an edit", response.Code)
	}
}

// The browser's answer is a redirect back to the list it came from, so the page
// works with scripts disabled.
func TestModerationPageRedirectsAfterADecision(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	rule := seedMemory(t, h, "Clock skew up to 60 seconds is intentional")
	handler := moderationHandler(t, h, &textEmbedder{}, adminToken)

	response := moderatedRequest(t, handler, http.MethodPost, "/api/moderation/"+rule.ID+"/approve", adminToken,
		url.Values{"rule_text": {"Clock skew up to 60 seconds is intentional"}, "status_filter": {"PENDING"}}, "text/html,application/xhtml+xml")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", response.Code, response.Body.String())
	}
	location := response.Header().Get("Location")
	if !strings.Contains(location, "decided=approved") || !strings.Contains(location, "status=PENDING") {
		t.Errorf("Location = %q, want the list it came from", location)
	}
}

// An unknown rule is not found rather than silently accepted.
func TestModerationReportsAnUnknownRule(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	handler := moderationHandler(t, h, &textEmbedder{}, adminToken)

	response := moderatedRequest(t, handler, http.MethodPost, "/api/moderation/00000000-0000-0000-0000-000000000000/reject",
		adminToken, url.Values{}, "application/json")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", response.Code, response.Body.String())
	}
}

// The CLIs embedding client and the service's embedder are the same interface,
// so serve wires the configured provider straight in.
var _ moderation.Embedder = (*embedder.Client)(nil)

// A page rendered with no rules says so rather than failing.
func TestModerationPageWithNoRules(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})

	response := moderatedRequest(t, moderationHandler(t, h, &textEmbedder{}, adminToken), http.MethodGet, "/ui/moderation", adminToken, nil, "text/html")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("No rules match")) {
		t.Errorf("the empty page does not say so:\n%s", response.Body.String())
	}
}
