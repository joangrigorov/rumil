package server

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
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

// Routes mounts the moderation surface.
func (m *ModerationUI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/moderation", m.authorize(m.list))
	mux.HandleFunc("POST /api/moderation/{id}/approve", m.authorize(m.approve))
	mux.HandleFunc("POST /api/moderation/{id}/reject", m.authorize(m.reject))
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
			http.Error(w, "the moderation surface needs moderation.admin_token", http.StatusServiceUnavailable)
			return
		}
		if !m.authorized(r) {
			m.logger.Warn("refused a moderation request with a missing or wrong token",
				"path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Basic realm="rumil moderation"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// authorized compares the presented credential with the configured token in
// constant time, so the comparison says nothing about how much of the token was
// right.
func (m *ModerationUI) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(header, "Bearer "):
		return equalToken(strings.TrimPrefix(header, "Bearer "), m.token)
	case strings.HasPrefix(header, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
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

func equalToken(presented, token string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func (m *ModerationUI) list(w http.ResponseWriter, r *http.Request) {
	filter := store.MemoryFilter{
		Status:  strings.ToUpper(r.URL.Query().Get("status")),
		Limit:   store.MemoryListMax,
		OrgName: r.URL.Query().Get("org"),
	}
	if repo := r.URL.Query().Get("repo"); repo != "" {
		org, name, found := strings.Cut(repo, "/")
		if !found {
			http.Error(w, `repo takes owner/name`, http.StatusBadRequest)
			return
		}
		filter.OrgName, filter.RepoName = org, name
	}

	memories, err := m.service.List(r.Context(), filter)
	if err != nil {
		m.logger.Error("listing the recorded rules failed", "error", err)
		http.Error(w, "reading the recorded rules failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := moderationTemplate.Execute(w, moderationPage{
		Rules:   memories,
		Status:  filter.Status,
		Repo:    r.URL.Query().Get("repo"),
		Decided: r.URL.Query().Get("decided"),
	}); err != nil {
		// The response is already streaming, so the failure can only be logged.
		m.logger.Error("rendering the moderation page failed", "error", err)
	}
}

func (m *ModerationUI) approve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	glob := r.PostFormValue("path_glob")
	memory, err := m.service.Approve(r.Context(), r.PathValue("id"), moderation.Decision{
		RuleText:  r.PostFormValue("rule_text"),
		PathGlob:  &glob,
		Moderator: moderatorOf(r),
	})
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	m.decide(w, r, memory, "approved")
}

func (m *ModerationUI) reject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	memory, err := m.service.Reject(r.Context(), r.PathValue("id"), moderatorOf(r))
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	m.decide(w, r, memory, "rejected")
}

func (m *ModerationUI) decide(w http.ResponseWriter, r *http.Request, memory store.Memory, decision string) {
	// A browser form post cannot set Accept, so it is answered with a redirect
	// back to the list; an API client that asks for JSON gets JSON.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		target := "/ui/moderation?decided=" + decision + "+" + memory.ShortID()
		if status := r.FormValue("status_filter"); status != "" {
			target += "&status=" + status
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
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

// statusFor maps a service error onto a status code: a rule that is not there
// is not found, and a decision the service refuses is a conflict.
func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrMemoryNotFound):
		return http.StatusNotFound
	case errors.Is(err, moderation.ErrNoEmbedder):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// moderatorOf names the decision in the audit line. The credential is shared,
// so the surface cannot know who used it, and it says so rather than guessing
// from a hostname or a login it does not have.
func moderatorOf(r *http.Request) string {
	if name := strings.TrimSpace(r.FormValue("moderator")); name != "" {
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
button { font: inherit; padding: .3rem .8rem; }
.meta { color: #555; font-size: .85rem; }
.notice { background: #eef7ee; border: 1px solid #b7d7b7; padding: .5rem 1rem; }
</style>
</head>
<body>
<h1>Recorded rules</h1>
<p class="meta">Only an approved rule is retrieved into a review. Editing a rule re-embeds it, so it is found by the words you approve.</p>
{{if .Decided}}<p class="notice">{{.Decided}}</p>{{end}}
<form method="get" action="/ui/moderation">
  <label>status <input type="text" name="status" value="{{.Status}}" placeholder="PENDING"></label>
  <label>repo <input type="text" name="repo" value="{{.Repo}}" placeholder="acme/payments"></label>
  <button type="submit">Filter</button>
</form>
<table>
<thead><tr><th>Rule</th><th>Repository</th><th>Status</th><th>Asked by</th><th>When</th><th>Decision</th></tr></thead>
<tbody>
{{range .Rules}}
<tr class="{{if eq .Status "PENDING"}}pending{{end}}">
  <td>
    <form id="approve-{{short .ID}}" method="post" action="/api/moderation/{{.ID}}/approve">
      <input type="hidden" name="status_filter" value="{{$.Status}}">
      <textarea name="rule_text">{{.RuleText}}</textarea><br>
      <label class="meta">glob <input type="text" name="path_glob" value="{{.PathGlob}}"></label>
      <div class="meta">why: {{.Rationale}}</div>
    </form>
    <div class="meta">{{short .ID}}{{if .ReviewedBy}} · decided by {{.ReviewedBy}}{{end}}</div>
  </td>
  <td>{{.OrgName}}/{{.RepoName}}<div class="meta">{{.Scope}}</div></td>
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
