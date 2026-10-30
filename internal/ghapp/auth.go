package ghapp

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v69/github"

	"github.com/joangrigorov/rumil/internal/config"
)

// ErrAppNotInstalled reports that the configured installation does not exist
// for the operator's GitHub App.
var ErrAppNotInstalled = errors.New("github app not installed for installation")

// ErrInvalidPrivateKey reports a private key that cannot be used to sign an
// app JWT.
var ErrInvalidPrivateKey = errors.New("invalid github app private key")

// ErrInstallationNotAllowed reports an installation id the configuration does
// not list. The App JWT may be able to see it, but this process does not serve
// it.
var ErrInstallationNotAllowed = errors.New("github installation is not configured")

// AppAuth mints and caches installation access tokens for the installations of
// one GitHub App this process serves. Each installation gets its own transport,
// which ghinstallation refreshes on its own; the set of ids is the allowlist,
// so a token is never minted for an account the operator did not name.
type AppAuth struct {
	apps      *ghinstallation.AppsTransport
	baseURL   string
	defaultID int64
	allowed   map[int64]bool

	mu         sync.Mutex
	transports map[int64]*ghinstallation.Transport
}

// NewAppAuth builds an authenticator from the configured credentials. baseURL
// overrides the GitHub API endpoint and is empty outside tests.
func NewAppAuth(cfg config.GitHub, keyPEM []byte, baseURL string) (*AppAuth, error) {
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}

	appsTransport := ghinstallation.NewAppsTransportFromPrivateKey(http.DefaultTransport, cfg.AppID, key)
	if baseURL != "" {
		appsTransport.BaseURL = baseURL
	}

	allowed := make(map[int64]bool, len(cfg.Installations)+1)
	for _, id := range cfg.InstallationIDs() {
		allowed[id] = true
	}

	return &AppAuth{
		apps:       appsTransport,
		baseURL:    baseURL,
		defaultID:  cfg.PrimaryInstallationID(),
		allowed:    allowed,
		transports: make(map[int64]*ghinstallation.Transport),
	}, nil
}

// resolve turns an optional installation id into a configured one. Zero means
// the primary installation, which is the single-installation form's id.
func (a *AppAuth) resolve(id int64) (int64, error) {
	if id <= 0 {
		id = a.defaultID
	}
	if id <= 0 {
		return 0, errors.New("github has no installation to authenticate as")
	}
	if !a.allowed[id] {
		return 0, fmt.Errorf("%w: %d", ErrInstallationNotAllowed, id)
	}
	return id, nil
}

// transportFor returns the cached transport of one installation, creating it on
// first use. The transport is what caches and rotates that installation's
// token.
func (a *AppAuth) transportFor(id int64) (*ghinstallation.Transport, error) {
	id, err := a.resolve(id)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if transport := a.transports[id]; transport != nil {
		return transport, nil
	}
	transport := ghinstallation.NewFromAppsTransport(a.apps, id)
	if a.baseURL != "" {
		transport.BaseURL = a.baseURL
	}
	a.transports[id] = transport
	return transport, nil
}

// Verify mints the primary installation's token immediately, so a missing
// installation fails before any LLM session is initialized.
func (a *AppAuth) Verify(ctx context.Context) error {
	return a.VerifyInstallation(ctx, 0)
}

// VerifyInstallation mints one installation's token. Zero verifies the primary
// installation.
func (a *AppAuth) VerifyInstallation(ctx context.Context, id int64) error {
	id, err := a.resolve(id)
	if err != nil {
		return err
	}
	transport, err := a.transportFor(id)
	if err != nil {
		return err
	}
	if _, err := transport.Token(ctx); err != nil {
		var httpErr *ghinstallation.HTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil {
			switch httpErr.Response.StatusCode {
			case http.StatusNotFound, http.StatusForbidden:
				return fmt.Errorf("%w: %d", ErrAppNotInstalled, id)
			}
		}
		return fmt.Errorf("minting installation token for installation %d: %w", id, err)
	}
	return nil
}

// VerifyAll mints a token for every configured installation, which is what the
// server does before it accepts deliveries for any of them.
func (a *AppAuth) VerifyAll(ctx context.Context) error {
	a.mu.Lock()
	ids := make([]int64, 0, len(a.allowed))
	for id := range a.allowed {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if err := a.VerifyInstallation(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Client returns a go-github client authenticated as the primary installation,
// or nil when no installation is configured. ClientFor is the per-installation
// form and reports the error instead.
func (a *AppAuth) Client() *github.Client {
	client, _ := a.ClientFor(0)
	return client
}

// ClientFor returns a go-github client authenticated as one installation.
func (a *AppAuth) ClientFor(id int64) (*github.Client, error) {
	transport, err := a.transportFor(id)
	if err != nil {
		return nil, err
	}
	return apiClient(transport, a.baseURL), nil
}

// AppRecord is what the App's own record says about it: the login people
// mention in comments, and the permissions it holds.
type AppRecord struct {
	// Slug is the App's login, which is what its reviews are attributed to.
	Slug string
	// Permissions is the permission set GitHub reports for the App.
	Permissions *github.InstallationPermissions
}

// Record reads what the installation this process authenticates as is allowed
// to do.
//
// The read is the installation's own record rather than the App's: the App's
// permission settings can be ahead of the installation's, because a permission
// added to the App does not reach an installation until its owner accepts it,
// and an installation can be narrower than the App. `GET /app/installations/{id}`
// is app-authenticated rather than installation-authenticated, so it works
// before any token is minted, and it answers with both the App's slug — the
// login people mention in comments — and the permission set the tokens carry.
func (a *AppAuth) Record(ctx context.Context) (AppRecord, error) {
	return a.RecordFor(ctx, 0)
}

// RecordFor reads one installation's own record. Zero reads the primary
// installation.
func (a *AppAuth) RecordFor(ctx context.Context, id int64) (AppRecord, error) {
	id, err := a.resolve(id)
	if err != nil {
		return AppRecord{}, err
	}
	installation, _, err := apiClient(a.apps, a.baseURL).Apps.GetInstallation(ctx, id)
	if err != nil {
		return AppRecord{}, fmt.Errorf("reading installation %d's own record: %w", id, err)
	}
	slug := installation.GetAppSlug()
	if slug == "" {
		return AppRecord{}, fmt.Errorf("installation %d's record carries no App slug", id)
	}
	return AppRecord{Slug: slug, Permissions: installation.Permissions}, nil
}

// apiClient points a go-github client at the configured API endpoint, which is
// the public one unless a test overrode it.
func apiClient(transport http.RoundTripper, baseURL string) *github.Client {
	client := github.NewClient(&http.Client{Transport: transport})
	if baseURL != "" {
		if parsed, err := url.Parse(strings.TrimRight(baseURL, "/") + "/"); err == nil {
			client.BaseURL = parsed
		}
	}
	return client
}

func parseRSAPrivateKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block found", ErrInvalidPrivateKey)
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPrivateKey, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: key is %T, want *rsa.PrivateKey", ErrInvalidPrivateKey, parsed)
	}
	return key, nil
}
