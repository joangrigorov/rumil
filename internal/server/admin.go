package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// contentSecurityPolicy is what the operator pages send with every HTML
// answer. The pages carry no inline style or script, so the policy can name
// the origin alone; a future inline block would be refused by the browser
// rather than silently trusted.
const contentSecurityPolicy = "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'"

// adminCookieName carries the admin token to the browser's EventSource. The
// deck's live stream is fetched by EventSource, which cannot set an
// Authorization header, so the page hands the browser a same-site cookie.
const adminCookieName = "rumil_admin"

// adminAuth is the credential and the request checks the operator surfaces
// share. The token is accepted as a bearer credential, and as the password of
// a basic credential, because a browser cannot set an Authorization header of
// its own; basic auth is what lets a maintainer use the HTML pages without a
// login route. Reads also accept the cookie the pages set, which is what lets
// EventSource follow a session; a state-changing request still needs the
// header.
type adminAuth struct {
	token  string
	logger *slog.Logger
	realm  string
}

// newAdminAuth returns the guard for one surface. A nil logger discards.
func newAdminAuth(token, realm string, logger *slog.Logger) *adminAuth {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &adminAuth{token: token, logger: logger, realm: realm}
}

// authorize refuses a request that does not carry the token, before the handler
// reads anything.
func (a *adminAuth) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" {
			a.logger.Warn("an admin request arrived while the admin token is not set")
			a.refuse(w, r, http.StatusServiceUnavailable, "this surface needs an admin token")
			return
		}
		if !a.authorized(r) {
			a.logger.Warn("refused an admin request with a missing or wrong token",
				"path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Basic realm="`+a.realm+`"`)
			a.refuse(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		a.remember(w, r)
		next(w, r)
	}
}

// remember hands the browser a cookie for the reads that cannot carry a
// header. It is set on a read the operator already authenticated, and it is
// scoped to this site; a form or fetch on another origin does not send it.
func (a *adminAuth) remember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(a.token)),
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteStrictMode,
	})
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
// The token is accepted as the password of a basic credential so a maintainer
// can use the HTML page, and a browser attaches a cached basic credential to
// every request to this origin — including a form on somebody else's page. A
// cross-site form post is a CORS-safelisted content type, so it is sent without
// a preflight and the page's answer never has to be read for the write to
// happen. Nothing in the credential itself tells the two requests apart, so the
// request has to. A refusal is logged: it is the one decision event that says
// somebody else's page tried to decide.
//
// Sec-Fetch-Site and Origin are set by the browser and a page cannot forge
// them. A request carrying neither was not sent by a browser on another site —
// it is the CLI, or a script that sets its own headers and could have chosen
// any credential anyway — so it is allowed.
func (a *adminAuth) sameSite(next http.HandlerFunc) http.HandlerFunc {
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

// authorized checks the presented credential against the configured token. The
// scheme is matched case-insensitively because the RFC says it is
// case-insensitive, and a credential that fails to parse is simply not
// accepted. A read may present the cookie the pages set; every other method
// needs the header, so a cookie cannot drive a state change.
func (a *adminAuth) authorized(r *http.Request) bool {
	if a.authorizedHeader(r) {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return a.authorizedCookie(r)
}

func (a *adminAuth) authorizedHeader(r *http.Request) bool {
	scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found {
		return false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		return equalToken(credential, a.token)
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(credential)
		if err != nil {
			return false
		}
		_, password, found := strings.Cut(string(raw), ":")
		if !found {
			return false
		}
		return equalToken(password, a.token)
	default:
		return false
	}
}

func (a *adminAuth) authorizedCookie(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return false
	}
	return equalToken(string(raw), a.token)
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

// refuse answers a request the surface will not carry out, in the shape the
// caller asked for. One rule for the whole surface, so a refusal is never
// shaped differently from the success standing in for it: a client that asked
// for the page gets text, and everything else gets the JSON it is parsing.
func (a *adminAuth) refuse(w http.ResponseWriter, r *http.Request, status int, message string) {
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
