package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/server/web"
	"github.com/joangrigorov/rumil/internal/store"
)

// ModerationUI is the maintainer's surface: the rules waiting for a decision,
// and the two ways to decide. It is guarded by the configured admin credentials
// and has no login page and no session of its own.
type ModerationUI struct {
	service *moderation.Service
	auth    *AdminAuth
	logger  *slog.Logger
}

// ModerationOption tunes the moderation surface at construction.
type ModerationOption func(*ModerationUI)

// WithModerationAuth shares one sign-in guard with the other deck surfaces, so
// a session obtained from the login form works on all of them.
func WithModerationAuth(auth *AdminAuth) ModerationOption {
	return func(m *ModerationUI) { m.auth = auth }
}

// WithModerationUsers reads the admin users on each request, so a reloaded set
// takes effect without rebuilding the surface.
func WithModerationUsers(source func() []config.AdminUser) ModerationOption {
	return func(m *ModerationUI) {
		m.auth = NewAdminAuthSource(source, m.logger)
	}
}

// NewModerationUI builds the surface. An empty user list disables it rather
// than opening it: with no user there is no way to tell an operator from a
// stranger, so every request is refused.
func NewModerationUI(service *moderation.Service, users []config.AdminUser, logger *slog.Logger, opts ...ModerationOption) *ModerationUI {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ui := &ModerationUI{
		service: service,
		auth:    NewAdminAuth(users, logger),
		logger:  logger,
	}
	for _, opt := range opts {
		opt(ui)
	}
	return ui
}

// Routes mounts the moderation surface. The two state-changing routes carry the
// same-site check, which is what a browser cannot be asked to skip.
func (m *ModerationUI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/moderation", m.auth.authorize(m.list))
	mux.HandleFunc("GET /ui/moderation/{id}", m.auth.authorize(m.detail))
	mux.HandleFunc("POST /api/moderation/{id}/approve", m.auth.authorize(m.auth.sameSite(m.approve)))
	mux.HandleFunc("POST /api/moderation/{id}/reject", m.auth.authorize(m.auth.sameSite(m.reject)))
}

func (m *ModerationUI) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rawRepo := query.Get("repo")
	status := strings.ToUpper(strings.TrimSpace(query.Get("status")))
	scope := strings.ToUpper(strings.TrimSpace(query.Get("scope")))
	search := strings.TrimSpace(query.Get("search"))

	filter := store.MemoryFilter{
		Status:  status,
		Scope:   scope,
		Search:  search,
		Limit:   store.MemoryListMax,
		OrgName: query.Get("org"),
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

	// The counts are informational, so a failure to read them does not take the
	// table down; the page simply renders without the status summary.
	var stats *store.MemoryStats
	if counts, statsErr := m.service.Stats(r.Context()); statsErr != nil {
		m.logger.Warn("counting the recorded rules failed", "error", statsErr)
	} else {
		stats = &counts
	}

	// The repository filter is a dropdown of the repositories that actually
	// have rules. A repository named in the URL is kept even when the last rule
	// in it is gone, so a bookmarked filter still says what it is.
	repositories, reposErr := m.service.Repositories(r.Context())
	if reposErr != nil {
		m.logger.Warn("listing the rule repositories failed", "error", reposErr)
	}
	if rawRepo != "" && !slices.Contains(repositories, rawRepo) {
		repositories = append([]string{rawRepo}, repositories...)
	}

	// The page renders text a commenter wrote, so it is not framed: an approve
	// button under someone else's overlay would otherwise be a decision taken
	// by whoever drew the overlay, from this origin, which the same-site check
	// on the post cannot see.
	m.render(w, moderationTemplates, moderationPage{
		Title:        "Recorded rules",
		Section:      "rules",
		User:         adminIdentity(r),
		Rules:        memories,
		Count:        len(memories),
		Stats:        stats,
		Status:       status,
		Scope:        scope,
		Search:       search,
		Repo:         rawRepo,
		Repositories: repositories,
		Decided:      query.Get("decided"),
	})
}

func (m *ModerationUI) detail(w http.ResponseWriter, r *http.Request) {
	rule, err := m.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.render(w, moderationDetailTemplates, moderationDetailPage{
		Title:   "Rule " + rule.ShortID(),
		Section: "rules",
		User:    adminIdentity(r),
		Rule:    rule,
		Pending: rule.Status == store.MemoryPending,
		Decided: r.URL.Query().Get("decided"),
	})
}

// render writes one operator page with the headers every rules screen carries.
func (m *ModerationUI) render(w http.ResponseWriter, templates *template.Template, page any) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "layout", page); err != nil {
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
		Authority: r.PostFormValue("authority"),
		Moderator: moderatorOf(r),
	})
	if err != nil {
		m.fail(w, r, err)
		return
	}
	decision := "approved"
	if r.PostFormValue("intent") == "edit" {
		// Saving an approved rule keeps its status; the notice says what the
		// moderator did rather than calling it a fresh approval.
		decision = "updated"
	}
	m.decide(w, r, memory, decision)
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
			m.auth.refuse(w, r, http.StatusUnsupportedMediaType,
				"a decision is sent as application/x-www-form-urlencoded")
			return false
		}
	}
	if err := r.ParseForm(); err != nil {
		m.auth.refuse(w, r, http.StatusBadRequest, "malformed form")
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
		target := "/ui/moderation"
		if r.FormValue("next") == "detail" {
			// A decision taken on the details page returns there, so the
			// moderator sees the rule as it now stands instead of losing it
			// from a filtered table.
			target = "/ui/moderation/" + memory.ID
		}
		http.Redirect(w, r, target+"?"+query.Encode(), http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"id":        memory.ID,
		"short_id":  memory.ShortID(),
		"status":    memory.Status,
		"scope":     memory.Scope,
		"authority": memory.Authority,
		"rule":      memory.RuleText,
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
	m.auth.refuse(w, r, status, message)
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
	// whose text moved under the decision, an edit with no embedder, a decision
	// that would widen a rule to the whole organization, and a promotion of a
	// rule that still reaches it.
	case errors.Is(err, store.ErrAmbiguousMemory), errors.Is(err, store.ErrMemoryChanged),
		errors.Is(err, moderation.ErrNoEmbedder), errors.Is(err, moderation.ErrScopeWidened),
		errors.Is(err, moderation.ErrAuthorityWidened):
		return http.StatusConflict
	// A status, a scope or an authority that is not one a rule can have is a bad
	// request.
	case errors.Is(err, moderation.ErrUnknownStatus), errors.Is(err, moderation.ErrUnknownScope),
		errors.Is(err, moderation.ErrUnknownAuthority):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
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
	// The signed-in user is what the surface knows about the holder.
	if name := adminIdentity(r); name != "" {
		return name
	}
	return "admin"
}

// moderationPage is what the rules table renders. Stats is nil when the counts
// could not be read, which only costs the status summary.
type moderationPage struct {
	Title        string
	Section      string
	Refresh      string
	Fixed        bool
	User         string
	Rules        []store.Memory
	Count        int
	Stats        *store.MemoryStats
	Status       string
	Scope        string
	Search       string
	Repo         string
	Repositories []string
	Decided      string
}

// moderationDetailPage is the view model of one rule: everything the table has
// no room for, and the decision form when the rule is still pending.
type moderationDetailPage struct {
	Title   string
	Section string
	Refresh string
	Fixed   bool
	User    string
	Rule    store.Memory
	Pending bool
	Decided string
}

// moderationTemplates is the page set: the shared layout and the moderation
// content. Every action is a plain form post, so the page works with scripts
// disabled — which matters on a surface that can promote unvetted text into a
// review prompt, where the less machinery between the operator and the decision
// the better.
// The table and the details screen are separate sets: both define "content",
// which the shared layout renders, so one set can only hold one of them.
var (
	moderationTemplates       = parseTemplates("moderation.html", pageFuncs())
	moderationDetailTemplates = parseTemplates("moderation_detail.html", pageFuncs())
)

// pageFuncs are the helpers every operator page shares.
func pageFuncs() template.FuncMap {
	return template.FuncMap{
		"short": func(id string) string {
			if i := strings.IndexByte(id, '-'); i > 0 {
				return id[:i]
			}
			return id
		},
		"date": func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 UTC") },
		"datePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.UTC().Format("2 Jan 2006, 15:04 UTC")
		},
		"agePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return ageText(*t)
		},
		"clock": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.UTC().Format("15:04")
		},
		"lower":    strings.ToLower,
		"duration": durationText,
		"sha":      shortSHA,
		"tokens":   tokenText,
		"usd":      usdText,
		"age":      ageText,
	}
}

// ageText is how long ago something was created, which is what the queue shows
// for a waiting job.
func ageText(created time.Time) string {
	elapsed := time.Since(created)
	if elapsed < time.Second {
		return "<1s"
	}
	return elapsed.Round(time.Second).String()
}

// tokenText groups a token count in thousands, which is how a bill reads.
func tokenText(count int64) string {
	digits := strconv.FormatInt(count, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return sign + b.String()
}

// usdText is an estimated cost. Sub-cent amounts keep four decimals, so a cheap
// review does not read as free.
func usdText(cost float64) string {
	if cost >= 1 {
		return fmt.Sprintf("$%.2f", cost)
	}
	return fmt.Sprintf("$%.4f", cost)
}

// durationText is how long a session has run, or ran.
func durationText(start time.Time, end *time.Time) string {
	if end == nil {
		return "running"
	}
	elapsed := end.Sub(start)
	if elapsed < time.Second {
		return "<1s"
	}
	return elapsed.Round(time.Second).String()
}

// shortSHA is the seven-character form GitHub shows for a commit.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// parseTemplates builds one page set from the embedded layout and the named
// page. Each page gets its own set because every page defines "content".
func parseTemplates(page string, funcs template.FuncMap) *template.Template {
	return template.Must(template.New(page).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/"+page))
}
