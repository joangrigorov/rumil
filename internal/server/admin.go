package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/joangrigorov/rumil/internal/config"
)

// contentSecurityPolicy is what the operator pages send with every HTML
// answer. The pages carry no inline style or script, so the policy can name
// the origin alone; a future inline block would be refused by the browser
// rather than silently trusted.
const contentSecurityPolicy = "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'"

const (
	// sessionCookieName is the login session. It is HttpOnly and SameSite=Lax,
	// so a cross-site form cannot ride it, and it is sent on a top-level
	// navigation from another site, which a Strict cookie would not be.
	sessionCookieName = "rumil_session"
	// loginCSRFCookieName pairs the form's hidden field with this request. The
	// login route is public, so the token is what tells a form rendered here
	// from one an attacker's page posted.
	loginCSRFCookieName = "rumil_login_csrf"

	sessionVersion = 1
	// sessionTTL bounds a sign-in that is not remembered. The browser also
	// drops the cookie when it closes, because no Max-Age is set.
	sessionTTL = 12 * time.Hour
	// rememberedSessionTTL is the lifetime of a session whose holder asked to
	// stay signed in.
	rememberedSessionTTL = 30 * 24 * time.Hour
	loginCSRFTTL         = 10 * time.Minute
)

// adminIdentityKey carries the name of the user a request authenticated with.
// The audit trail reads it instead of a shared credential.
type adminIdentityKey struct{}

// adminIdentity is the user name that authorized a request, or an empty string
// when the request did not pass through the guard.
func adminIdentity(r *http.Request) string {
	name, _ := r.Context().Value(adminIdentityKey{}).(string)
	return name
}

// AdminAuth is the deck's sign-in guard. Users are read on every request, so a
// reloaded list takes effect without rebuilding the surfaces.
//
// A session is a signed cookie rather than an entry in memory: the signature
// key is derived from the configured names and password hashes, so a remembered
// session survives a restart and stops working when any credential changes.
type AdminAuth struct {
	users  func() []config.AdminUser
	logger *slog.Logger
}

// NewAdminAuth returns the guard for one fixed user list.
func NewAdminAuth(users []config.AdminUser, logger *slog.Logger) *AdminAuth {
	return NewAdminAuthSource(func() []config.AdminUser { return users }, logger)
}

// NewAdminAuthSource returns the guard whose user list may change between
// requests.
func NewAdminAuthSource(users func() []config.AdminUser, logger *slog.Logger) *AdminAuth {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &AdminAuth{users: users, logger: logger}
}

// WithLogin mounts the sign-in and sign-out routes in front of next. The deck's
// own routes still authorize every request; these two are the way a browser
// obtains and drops the session cookie.
func WithLogin(next http.Handler, auth *AdminAuth) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/login", auth.login)
	mux.HandleFunc("POST /ui/login", auth.signIn)
	mux.HandleFunc("POST /ui/logout", auth.authorize(auth.sameSite(auth.signOut)))
	mux.Handle("/", next)
	return mux
}

// authorize refuses a request that does not carry a session or a basic
// credential. A browser asking for a page is sent to the login form; an API
// client gets a 401 it can read.
func (a *AdminAuth) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users := a.users()
		if len(users) == 0 {
			a.logger.Warn("an admin request arrived while no admin user is configured")
			a.refuse(w, r, http.StatusServiceUnavailable, "this surface needs a configured admin user")
			return
		}
		if name, ok := a.sessionUser(r, users); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), adminIdentityKey{}, name)))
			return
		}
		if name, ok := a.basicUser(r, users); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), adminIdentityKey{}, name)))
			return
		}
		if wantsPage(r) {
			http.Redirect(w, r, loginHref(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		a.unauthorized(w, r)
	}
}

func (a *AdminAuth) unauthorized(w http.ResponseWriter, r *http.Request) {
	a.logger.Warn("refused an admin request without a valid session or password",
		"path", r.URL.Path, "remote", r.RemoteAddr)
	a.refuse(w, r, http.StatusUnauthorized, "unauthorized")
}

// login renders the sign-in form. A browser that already holds a session is
// sent on to where it was going.
func (a *AdminAuth) login(w http.ResponseWriter, r *http.Request) {
	if len(a.users()) == 0 {
		a.renderLogin(w, r, http.StatusServiceUnavailable, "No admin users are configured.", "", "")
		return
	}
	next := nextPath(r.URL.Query().Get("next"))
	if _, ok := a.sessionUser(r, a.users()); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	a.renderLogin(w, r, http.StatusOK, "", "", next)
}

// signIn checks the form and, on success, hands the browser a session cookie.
func (a *AdminAuth) signIn(w http.ResponseWriter, r *http.Request) {
	users := a.users()
	if len(users) == 0 {
		a.renderLogin(w, r, http.StatusServiceUnavailable, "No admin users are configured.", "", "")
		return
	}
	if !sameOrigin(r) {
		a.logger.Warn("refused a login a browser reported as cross-site",
			"remote", r.RemoteAddr, "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "origin", r.Header.Get("Origin"))
		a.renderLogin(w, r, http.StatusForbidden, "The sign-in form has to come from this deck.", "", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		a.renderLogin(w, r, http.StatusBadRequest, "The sign-in form could not be read.", "", "")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	remember := r.PostFormValue("remember") != ""
	next := nextPath(r.PostFormValue("next"))

	if !a.validLoginCSRF(r) {
		a.renderLogin(w, r, http.StatusBadRequest, "The sign-in form expired. Try again.", username, next)
		return
	}
	if !verifyUser(users, username, password) {
		a.logger.Warn("refused a deck login", "user", username, "remote", r.RemoteAddr)
		// A failed attempt gets a fresh form token, so a retry is a new form.
		a.renderLogin(w, r, http.StatusUnauthorized, "Wrong username or password.", username, next)
		return
	}

	a.setSession(w, r, username, remember)
	a.clearLoginCSRF(w, r)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// signOut drops the cookie. The signature is stateless, so this is what tells
// the browser to forget it.
func (a *AdminAuth) signOut(w http.ResponseWriter, r *http.Request) {
	a.clearSession(w, r)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// renderLogin writes the form and hands it a CSRF token paired with a
// short-lived cookie.
func (a *AdminAuth) renderLogin(w http.ResponseWriter, r *http.Request, status int, message, username, next string) {
	token := newToken()
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    token,
		Path:     "/ui/login",
		MaxAge:   int(loginCSRFTTL.Seconds()),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := loginTemplates.ExecuteTemplate(w, "layout", loginPage{
		Title:    "Sign in",
		Section:  "login",
		Error:    message,
		Next:     next,
		Username: username,
		CSRF:     token,
	}); err != nil {
		a.logger.Error("rendering the login page failed", "error", err)
	}
}

func (a *AdminAuth) validLoginCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(loginCSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	form := r.PostFormValue("csrf")
	if form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(form)) == 1
}

func (a *AdminAuth) setSession(w http.ResponseWriter, r *http.Request, username string, remember bool) {
	ttl := sessionTTL
	if remember {
		ttl = rememberedSessionTTL
	}
	expires := time.Now().Add(ttl)
	value := a.sealSession(sessionClaims{
		Version:  sessionVersion,
		User:     username,
		Expires:  expires.Unix(),
		Remember: remember,
	}, a.users())
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	}
	if remember {
		cookie.MaxAge = int(ttl.Seconds())
		cookie.Expires = expires
	}
	http.SetCookie(w, cookie)
}

func (a *AdminAuth) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *AdminAuth) clearLoginCSRF(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    "",
		Path:     "/ui/login",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// sessionClaims is the payload inside the signed cookie. It names the user and
// when the session ends; the signature covers both.
type sessionClaims struct {
	Version  int    `json:"v"`
	User     string `json:"u"`
	Expires  int64  `json:"e"`
	Remember bool   `json:"r,omitempty"`
}

// sealSession marshals and signs one session cookie value. The signing key
// belongs to the named user, so changing somebody else's password leaves this
// session alone.
func (a *AdminAuth) sealSession(claims sessionClaims, users []config.AdminUser) string {
	user, ok := findUser(users, claims.User)
	if !ok {
		return ""
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		// The struct holds only strings and integers.
		return ""
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, userSessionKey(user))
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sessionUser returns the user a request's cookie names, when the signature,
// the expiry and the configured user list all agree.
func (a *AdminAuth) sessionUser(r *http.Request, users []config.AdminUser) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	encoded, signature, found := strings.Cut(cookie.Value, ".")
	if !found || encoded == "" || signature == "" {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	user, ok := findUser(users, claims.User)
	if !ok {
		return "", false
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, userSessionKey(user))
	mac.Write([]byte(encoded))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", false
	}
	if claims.Version != sessionVersion || time.Now().Unix() >= claims.Expires {
		return "", false
	}
	return claims.User, true
}

// basicUser verifies a username and password presented as HTTP Basic. It is how
// a script and a metrics scraper sign in; a browser uses the form. Bearer
// tokens are deliberately not accepted.
func (a *AdminAuth) basicUser(r *http.Request, users []config.AdminUser) (string, bool) {
	scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(credential)
	if err != nil {
		return "", false
	}
	username, password, found := strings.Cut(string(raw), ":")
	if !found {
		return "", false
	}
	if !verifyUser(users, username, password) {
		return "", false
	}
	return username, true
}

// userSessionKey derives one user's signing key. A changed password hash
// changes that key, so the user's earlier sessions stop verifying without a
// session store to clean up, while another user's sessions keep working.
func userSessionKey(user config.AdminUser) []byte {
	digest := sha256.New()
	digest.Write([]byte("rumil/deck/session/v1\x00"))
	digest.Write([]byte(user.Name))
	digest.Write([]byte{0})
	digest.Write([]byte(user.PasswordHash))
	return digest.Sum(nil)
}

// verifyUser checks one username and password against every configured user.
// Every entry is compared, so the response time does not say whether a name
// exists: a missing user is compared against a hash of its own.
func verifyUser(users []config.AdminUser, username, password string) bool {
	if username == "" || password == "" {
		return false
	}
	name := ""
	found := false
	for _, user := range users {
		hash := user.PasswordHash
		candidate := password
		if user.Name != username {
			hash = string(dummyPasswordHash())
			candidate = "not-this-users-password"
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(candidate)) == nil {
			name = user.Name
			found = true
		}
	}
	if !found {
		// No configured name matched, so spend the same work once.
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash(), []byte(password))
		return false
	}
	return name == username
}

var (
	dummyHashOnce sync.Once
	dummyHash     []byte
)

// dummyPasswordHash is a valid bcrypt hash used to keep a missing user as
// expensive as a wrong password.
func dummyPasswordHash() []byte {
	dummyHashOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte("rumil-dummy-password"), bcrypt.DefaultCost)
	})
	return dummyHash
}

func findUser(users []config.AdminUser, name string) (config.AdminUser, bool) {
	for _, user := range users {
		if user.Name == name {
			return user, true
		}
	}
	return config.AdminUser{}, false
}

// newToken returns a URL-safe random value for the login CSRF cookie.
func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// loginHref sends a browser to the form and back to where it was going.
func loginHref(next string) string {
	return "/ui/login?next=" + url.QueryEscape(next)
}

// nextPath keeps a post-login redirect on this deck. A value that is absolute,
// names a host, or leaves /ui is replaced by Home.
func nextPath(value string) string {
	if value == "" {
		return "/ui/home"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/ui/") {
		return "/ui/home"
	}
	return value
}

// requestIsTLS reports whether the browser reached Rumil over TLS, directly or
// through a proxy that set X-Forwarded-Proto. A Secure cookie set over plain
// HTTP would be dropped by the browser, so the check has to see the proxy's
// view.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sameSite refuses a state-changing request that the browser says came from
// another site.
//
// A session cookie is attached by the browser to a cross-site form post, so
// the request has to be told apart from one this deck rendered. A refusal is
// logged: it is the one decision event that says somebody else's page tried to
// decide.
//
// Sec-Fetch-Site and Origin are set by the browser and a page cannot forge
// them. A request carrying neither was not sent by a browser on another site —
// it is the CLI, or a script that sets its own headers and could have chosen
// any credential anyway — so it is allowed.
func (a *AdminAuth) sameSite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			a.logger.Warn("refused a decision a browser reported as cross-site",
				"path", r.URL.Path, "remote", r.RemoteAddr,
				"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "origin", r.Header.Get("Origin"))
			a.refuse(w, r, http.StatusForbidden, "a decision has to come from the admin page")
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

// refuse answers a request the surface will not carry out, in the shape the
// caller asked for. One rule for the whole surface, so a refusal is never
// shaped differently from the success standing in for it: a client that asked
// for the page gets text, and everything else gets the JSON it is parsing.
func (a *AdminAuth) refuse(w http.ResponseWriter, r *http.Request, status int, message string) {
	if wantsHTML(r) {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		a.logger.Error("writing an admin refusal failed", "error", err)
	}
}

// wantsHTML reports whether the caller is a browser rendering the page, which
// is what decides the shape of both a decision and a refusal. A form post
// cannot set Accept on its own, and the answer to a decision is the list.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// wantsPage reports whether an unauthenticated request is a browser asking for
// one of the deck's pages, which is where a redirect to the login form belongs.
func wantsPage(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return wantsHTML(r) || strings.HasPrefix(r.URL.Path, "/ui/")
}

// loginPage is the sign-in form's view model.
type loginPage struct {
	Title    string
	Section  string
	Refresh  string
	Fixed    bool
	User     string
	Error    string
	Next     string
	Username string
	CSRF     string
}

var loginTemplates = parseTemplates("login.html", pageFuncs())
