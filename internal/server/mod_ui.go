package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
)

// ModerationUI is the maintainer's surface: the rules waiting for a decision,
// and the two ways to decide. It is guarded by moderation.admin_token and has
// no login page, no session and no cookie, so it is stateless by construction.
type ModerationUI struct {
	service *moderation.Service
	token   string
	logger  *slog.Logger
}

// NewModerationUI builds the surface. An empty token disables it rather than
// opening it: with no token there is no way to tell an operator from a
// stranger, so every request is refused.
func NewModerationUI(service *moderation.Service, token string, logger *slog.Logger) *ModerationUI {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &ModerationUI{service: service, token: token, logger: logger}
}

// Routes mounts the moderation surface. The two state-changing routes carry the
// same-site check, which is what a browser cannot be asked to skip.
func (m *ModerationUI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/moderation", m.authorize(m.list))
	mux.HandleFunc("POST /api/moderation/{id}/approve", m.authorize(m.sameSite(m.approve)))
	mux.HandleFunc("POST /api/moderation/{id}/reject", m.authorize(m.sameSite(m.reject)))
}

// sameSite refuses a state-changing request that the browser says came from
// another site.
//
// The token is accepted as the password of a basic credential so a maintainer
// can use the HTML page, and a browser attaches a cached basic credential to
// every request to this origin — including a form on somebody else's page. A
// cross-site form post is a CORS-safelisted content type, so it is sent without
// a preflight and the page's answer never has to be read for the write to
// happen. Nothing in the credential itself tells the two requests apart, so the
// request has to. A refusal is logged: it is the one moderation event that says
// somebody else's page tried to decide.
//
// Sec-Fetch-Site and Origin are set by the browser and a page cannot forge
// them. A request carrying neither was not sent by a browser on another site —
// it is the CLI, or a script that sets its own headers and could have chosen
// any credential anyway — so it is allowed.
func (m *ModerationUI) sameSite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			m.logger.Warn("refused a moderation decision a browser reported as cross-site",
				"path", r.URL.Path, "remote", r.RemoteAddr,
				"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "origin", r.Header.Get("Origin"))
			m.refuse(w, r, http.StatusForbidden, "a decision has to come from the moderation page")
			return
		}
		next(w, r)
	}
}

// sameOrigin reports whether the request is one a browser could have started
// here. "none" is a request the browser itself initiated, which is not a page
// driving a form; anything else is refused, including a sibling subdomain.
//
// The Origin is compared by host, port included, so the reverse proxy in front
// of this service has to pass the Host through unchanged. An Origin that names
// no host at all — the `null` a sandboxed frame or a `data:` URL sends — is
// refused rather than compared.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// authorize refuses a request that does not carry the token, before the handler
// reads anything. It accepts the token as a bearer credential, and as the
// password of a basic credential: a browser cannot set an Authorization header
// of its own, so basic auth is what lets a maintainer use the HTML page without
// a login route, a cookie, or a line of JavaScript.
func (m *ModerationUI) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.token == "" {
			m.logger.Warn("a moderation request arrived while moderation.admin_token is not set")
			m.refuse(w, r, http.StatusServiceUnavailable, "the moderation surface needs moderation.admin_token")
			return
		}
		if !m.authorized(r) {
			m.logger.Warn("refused a moderation request with a missing or wrong token",
				"path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Basic realm="rumil moderation"`)
			m.refuse(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// authorized checks the presented credential against the configured token. The
// scheme is matched case-insensitively because the RFC says it is
// case-insensitive, and a credential that fails to parse is simply not
// accepted.
func (m *ModerationUI) authorized(r *http.Request) bool {
	scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found {
		return false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		return equalToken(credential, m.token)
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(credential)
		if err != nil {
			return false
		}
		_, password, found := strings.Cut(string(raw), ":")
		if !found {
			return false
		}
		return equalToken(password, m.token)
	default:
		return false
	}
}

// equalToken compares a presented credential with the configured token. Both
// sides are hashed first so that the comparison is over two values of the same
// length: ConstantTimeCompare answers immediately when the lengths differ,
// which would otherwise report how long the configured token is.
func equalToken(presented, token string) bool {
	presentedSum := sha256.Sum256([]byte(presented))
	tokenSum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presentedSum[:], tokenSum[:]) == 1
}

func (m *ModerationUI) list(w http.ResponseWriter, r *http.Request) {
	rawRepo := r.URL.Query().Get("repo")
	filter := store.MemoryFilter{
		Status:  r.URL.Query().Get("status"),
		Limit:   store.MemoryListMax,
		OrgName: r.URL.Query().Get("org"),
	}
	if rawRepo != "" {
		org, name, err := splitRepoRef(rawRepo)
		if err != nil {
			http.Error(w, "repo takes owner/name", http.StatusBadRequest)
			return
		}
		filter.OrgName, filter.RepoName = org, name
	}

	memories, err := m.service.List(r.Context(), filter)
	if err != nil {
		m.fail(w, r, err)
		return
	}

	// The page renders text a commenter wrote, so it is not framed: an approve
	// button under someone else's overlay would otherwise be a decision taken
	// by whoever drew the overlay, from this origin, which the same-site check
	// on the post cannot see.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := moderationTemplate.Execute(w, moderationPage{
		Rules:   memories,
		Status:  strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))),
		Repo:    rawRepo,
		Decided: r.URL.Query().Get("decided"),
	}); err != nil {
		// The response is already streaming, so the failure can only be logged.
		m.logger.Error("rendering the moderation page failed", "error", err)
	}
}

func (m *ModerationUI) approve(w http.ResponseWriter, r *http.Request) {
	if !m.readForm(w, r) {
		return
	}
	memory, err := m.service.Approve(r.Context(), r.PathValue("id"), moderation.Decision{
		RuleText:  r.PostFormValue("rule_text"),
		PathGlob:  pathGlobOf(r),
		Scope:     r.PostFormValue("scope"),
		Moderator: moderatorOf(r),
	})
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.decide(w, r, memory, "approved")
}

func (m *ModerationUI) reject(w http.ResponseWriter, r *http.Request) {
	if !m.readForm(w, r) {
		return
	}
	memory, err := m.service.Reject(r.Context(), r.PathValue("id"), moderatorOf(r))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.decide(w, r, memory, "rejected")
}

// readForm parses the body a decision arrives in, and refuses a body it cannot
// read rather than reading nothing from it. ParseForm ignores a multipart body,
// so a caller that sent one would have its rule_text silently dropped and the
// rule approved as it happened to be stored, which is a decision nobody made.
// An empty body is the "approve as it was proposed" case, which the CLI has too.
func (m *ModerationUI) readForm(w http.ResponseWriter, r *http.Request) bool {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			m.refuse(w, r, http.StatusUnsupportedMediaType,
				"a decision is sent as application/x-www-form-urlencoded")
			return false
		}
	}
	if err := r.ParseForm(); err != nil {
		m.refuse(w, r, http.StatusBadRequest, "malformed form")
		return false
	}
	return true
}

// splitRepoRef splits owner/name the way the CLI's --repo flag does, so the page
// and the command agree on what a repository reference is: one slash, with
// something on each side of it.
func splitRepoRef(repo string) (owner, name string, err error) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("a repository reference is owner/name, not %q", repo)
	}
	return owner, name, nil
}

// pathGlobOf reads the glob the form carried. An absent field is not the same
// as an empty one: absent leaves the rule's glob alone, which is what the CLI
// does and what a client that only meant to edit the text expects, while an
// empty one widens the rule to the whole repository, which is what a maintainer
// clearing the box on the page is asking for.
func pathGlobOf(r *http.Request) *string {
	values, present := r.PostForm["path_glob"]
	if !present {
		return nil
	}
	glob := values[0]
	return &glob
}

// decide answers a recorded decision: a form post from a browser cannot set
// Accept, so it is sent back to the list it came from, and a client that asks
// for JSON gets JSON.
func (m *ModerationUI) decide(w http.ResponseWriter, r *http.Request, memory store.Memory, decision string) {
	if wantsHTML(r) {
		query := url.Values{"decided": {decision + " " + memory.ShortID()}}
		if status := r.FormValue("status_filter"); status != "" {
			query.Set("status", status)
		}
		http.Redirect(w, r, "/ui/moderation?"+query.Encode(), http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"id":       memory.ID,
		"short_id": memory.ShortID(),
		"status":   memory.Status,
		"rule":     memory.RuleText,
	}); err != nil {
		m.logger.Error("writing the moderation result failed", "error", err)
	}
}

// fail answers a refused decision. An error this surface recognizes is repeated,
// because it says what the caller can do about it; anything else is logged and
// answered with a fixed sentence, so a database message is not handed to a
// client.
func (m *ModerationUI) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := statusFor(err)
	message := err.Error()
	switch status {
	case http.StatusInternalServerError:
		m.logger.Error("a moderation request failed", "path", r.URL.Path, "error", err)
		message = "the decision could not be recorded"
	case http.StatusConflict:
		// A refusal to act on a rule as it is now is worth a line: it means a
		// second moderator is working the same rule, or a page was left open.
		m.logger.Warn("refused a moderation decision", "path", r.URL.Path, "error", err)
	}
	m.refuse(w, r, status, message)
}

// refuse answers a request the surface will not carry out, in the shape the
// caller asked for. One rule for the whole surface, so a refusal is never
// shaped differently from the success standing in for it: a client that asked
// for the page gets text, and everything else gets the JSON it is parsing.
func (m *ModerationUI) refuse(w http.ResponseWriter, r *http.Request, status int, message string) {
	if wantsHTML(r) {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		m.logger.Error("writing the moderation refusal failed", "error", err)
	}
}

// statusFor maps a service error onto a status code: a rule that is not there is
// not found, an ambiguous reference and a decision that lost a race both ask the
// moderator to look again, a refused edit is a conflict, and a filter that names
// no status is a bad request.
func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrMemoryNotFound):
		return http.StatusNotFound
	// Refusals a moderator has to act on are conflicts: an ambiguous id, a rule
	// whose text moved under the decision, an edit with no embedder, and a
	// decision that would widen a rule to the whole organization.
	case errors.Is(err, store.ErrAmbiguousMemory), errors.Is(err, store.ErrMemoryChanged),
		errors.Is(err, moderation.ErrNoEmbedder), errors.Is(err, moderation.ErrScopeWidened):
		return http.StatusConflict
	// A status or a scope that is not one a rule can have is a bad request.
	case errors.Is(err, moderation.ErrUnknownStatus), errors.Is(err, moderation.ErrUnknownScope):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// wantsHTML reports whether the caller is a browser rendering the page, which
// is what decides the shape of both a decision and a refusal. A form post
// cannot set Accept on its own, and the answer to a decision is the list.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// moderatorOf names the decision in the audit line. The credential is shared,
// so the surface cannot know who used it, and it says so rather than guessing
// from a hostname or a login it does not have.
func moderatorOf(r *http.Request) string {
	// The body, not r.FormValue: the query string is part of a URL that gets
	// pasted around, and an audit line is about the decision, not about a link.
	if name := strings.TrimSpace(r.PostFormValue("moderator")); name != "" {
		return name
	}
	return "admin-token"
}

// moderationPage is what the template renders.
type moderationPage struct {
	Rules   []store.Memory
	Status  string
	Repo    string
	Decided string
}

// moderationTemplate is the whole UI: a filtered table of rules, and one form
// per row. Every action is a plain form post, so the page works with scripts
// disabled — which matters on a surface that can promote unvetted text into a
// review prompt, where the less machinery between the operator and the decision
// the better.
var moderationTemplate = template.Must(template.New("moderation").Funcs(template.FuncMap{
	"short": func(id string) string {
		if i := strings.IndexByte(id, '-'); i > 0 {
			return id[:i]
		}
		return id
	},
	"date": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Rumil — recorded rules</title>
<style>
body { font-family: system-ui, sans-serif; margin: 2rem; color: #111; }
table { border-collapse: collapse; width: 100%; }
th, td { border-bottom: 1px solid #ddd; padding: .5rem; text-align: left; vertical-align: top; }
tr.pending { background: #fffbe6; }
form { margin: 0; }
textarea { width: 32rem; height: 4rem; font: inherit; }
input[type=text] { font: inherit; width: 12rem; }
select { font: inherit; }
button { font: inherit; padding: .3rem .8rem; }
.meta { color: #555; font-size: .85rem; }
.notice { background: #eef7ee; border: 1px solid #b7d7b7; padding: .5rem 1rem; }
.scope { display: inline-block; padding: .05rem .4rem; border-radius: .2rem; font-size: .8rem; font-weight: 600; }
.scope.repo { background: #eef1f5; color: #333; }
.scope.org { background: #fde9d9; color: #7a3c00; border: 1px solid #e8b98d; }
</style>
</head>
<body>
<h1>Recorded rules</h1>
<p class="meta">Only an approved rule is retrieved into a review. Editing a rule re-embeds it, so it is found by the words you approve. A rule scoped <span class="scope org">ORG</span> applies to every repository in the organization that has nothing more specific to say; a <span class="scope repo">REPO</span> rule applies to the repository it was recorded in and outranks the organization's when both match. An organization rule can be recorded for the repository it was proposed in instead; a repository rule is not widened to the organization from here.</p>
{{if .Decided}}<p class="notice">{{.Decided}}</p>{{end}}
<form method="get" action="/ui/moderation">
  <label>status <input type="text" name="status" value="{{.Status}}" placeholder="PENDING"></label>
  <label>repo <input type="text" name="repo" value="{{.Repo}}" placeholder="acme/payments"></label>
  <button type="submit">Filter</button>
</form>
<table>
<thead><tr><th>Rule</th><th>Applies to</th><th>Status</th><th>Asked by</th><th>When</th><th>Decision</th></tr></thead>
<tbody>
{{range .Rules}}
<tr class="{{if eq .Status "PENDING"}}pending{{end}}">
  <td>
    <form id="approve-{{short .ID}}" method="post" action="/api/moderation/{{.ID}}/approve">
      <input type="hidden" name="status_filter" value="{{$.Status}}">
      <textarea name="rule_text">{{.RuleText}}</textarea><br>
      <label class="meta">glob <input type="text" name="path_glob" value="{{.PathGlob}}"></label>
      {{if eq .Scope "ORG"}}<label class="meta">scope
        <select name="scope">
          <option value="ORG" selected>ORG — every repository in {{.OrgName}}</option>
          <option value="REPO">REPO — this repository, {{.RepoName}}</option>
        </select>
      </label>{{end}}
      <div class="meta">why: {{.Rationale}}</div>
    </form>
    <div class="meta">{{short .ID}}{{if .ReviewedBy}} · decided by {{.ReviewedBy}}{{end}}</div>
  </td>
  <td>
    {{if eq .Scope "ORG"}}<span class="scope org">ORG</span> {{.OrgName}}<div class="meta">proposed in {{.RepoName}}</div>
    {{else}}<span class="scope repo">REPO</span> {{.OrgName}}/{{.RepoName}}{{end}}
  </td>
  <td>{{.Status}}</td>
  <td>{{.RequestedBy}}</td>
  <td class="meta">{{date .CreatedAt}}</td>
  <td>
    <button type="submit" form="approve-{{short .ID}}">Approve</button>
    <form method="post" action="/api/moderation/{{.ID}}/reject">
      <input type="hidden" name="status_filter" value="{{$.Status}}">
      <button type="submit">Reject</button>
    </form>
  </td>
</tr>
{{else}}
<tr><td colspan="6">No rules match.</td></tr>
{{end}}
</tbody>
</table>
</body>
</html>
`))
