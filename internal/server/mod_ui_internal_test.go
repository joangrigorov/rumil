package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
)

// Every refusal the surface can make has to reach the caller as a status they
// can act on: 500 for a rule that needs a moderator to look again says the
// surface is broken rather than the decision.
func TestStatusForMapsWhatTheSurfaceCanRefuse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"an unknown rule", fmt.Errorf("%w: 9f1c", store.ErrMemoryNotFound), http.StatusNotFound},
		{"a shared prefix", fmt.Errorf("%w: 9f1c", store.ErrAmbiguousMemory), http.StatusConflict},
		{"a rule that moved", fmt.Errorf("%w: 9f1c", store.ErrMemoryChanged), http.StatusConflict},
		{"an edit without a provider", moderation.ErrNoEmbedder, http.StatusConflict},
		{"a status that is not one", fmt.Errorf("%w: %q", moderation.ErrUnknownStatus, "APPROVE"), http.StatusBadRequest},
		{"the database is gone", fmt.Errorf("listing memories: connection refused"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFor(tc.err); got != tc.want {
				t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A browser attaches a cached basic credential to a request to this origin
// whichever page started it, so the decision has to be told apart by where the
// request came from.
func TestSameOriginReadsWhatTheBrowserSays(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"the page itself", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"started by the browser", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"a form on another site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"a sibling subdomain", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"its own origin", map[string]string{"Origin": "http://example.com"}, true},
		{"another origin", map[string]string{"Origin": "https://evil.example"}, false},
		{"another port", map[string]string{"Origin": "http://example.com:8080"}, false},
		{"a malformed origin", map[string]string{"Origin": "://"}, false},
		{"no browser at all", nil, true},
		{"a script that sets its own headers", map[string]string{"User-Agent": "curl/8"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.com/api/moderation/9f1c/reject", nil)
			for name, value := range tc.headers {
				request.Header.Set(name, value)
			}
			if got := sameOrigin(request); got != tc.want {
				t.Errorf("sameOrigin = %v, want %v", got, tc.want)
			}
		})
	}
}

// An absent glob and an empty one are different decisions.
func TestPathGlobOfTellsAnAbsentFieldFromAnEmptyOne(t *testing.T) {
	form := func(values url.Values) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/api/moderation/9f1c/approve", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return request
	}

	absent := form(url.Values{"rule_text": {"a rule"}})
	if err := absent.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	if glob := pathGlobOf(absent); glob != nil {
		t.Errorf("an absent path_glob read as %q, want no change", *glob)
	}

	empty := form(url.Values{"rule_text": {"a rule"}, "path_glob": {""}})
	if err := empty.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	glob := pathGlobOf(empty)
	if glob == nil || *glob != "" {
		t.Errorf("an empty path_glob read as %v, want it widened", glob)
	}
}

// The credential is compared whole, and the scheme is the RFC's to define.
func TestAuthorizedAcceptsWhatTheRFCAllows(t *testing.T) {
	ui := NewModerationUI(nil, "s3cret-token", nil)
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"a bearer token", "Bearer s3cret-token", true},
		{"a lowercase scheme", "bearer s3cret-token", true},
		{"another token", "Bearer nope", false},
		{"a prefix of the token", "Bearer s3cret", false},
		{"a token with a prefix", "Bearer xs3cret-token", false},
		{"the token with a trailing space", "Bearer s3cret-token ", false},
		{"no credential", "", false},
		{"a scheme with nothing after it", "Bearer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/ui/moderation", nil)
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}
			if got := ui.authorized(request); got != tc.want {
				t.Errorf("authorized(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}
