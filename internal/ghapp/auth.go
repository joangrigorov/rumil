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
	"strings"

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

// AppAuth mints and rotates installation access tokens for one GitHub App
// installation. Rumil ships no centralized App, so the installation is always
// the one named in the configuration.
type AppAuth struct {
	installationID int64
	transport      *ghinstallation.Transport
	apps           *ghinstallation.AppsTransport
	baseURL        string
}

// NewAppAuth builds an authenticator from the configured credentials. baseURL
// overrides the GitHub API endpoint and is empty outside tests.
func NewAppAuth(cfg config.GitHub, keyPEM []byte, baseURL string) (*AppAuth, error) {
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}

	appsTransport := ghinstallation.NewAppsTransportFromPrivateKey(http.DefaultTransport, cfg.AppID, key)
	transport := ghinstallation.NewFromAppsTransport(appsTransport, cfg.InstallationID)
	if baseURL != "" {
		transport.BaseURL = baseURL
		appsTransport.BaseURL = baseURL
	}

	return &AppAuth{
		installationID: cfg.InstallationID,
		transport:      transport,
		apps:           appsTransport,
		baseURL:        baseURL,
	}, nil
}

// Verify mints an installation token immediately so that a missing
// installation fails before any LLM session is initialized.
func (a *AppAuth) Verify(ctx context.Context) error {
	if _, err := a.transport.Token(ctx); err != nil {
		var httpErr *ghinstallation.HTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil {
			switch httpErr.Response.StatusCode {
			case http.StatusNotFound, http.StatusForbidden:
				return fmt.Errorf("%w: %d", ErrAppNotInstalled, a.installationID)
			}
		}
		return fmt.Errorf("minting installation token for installation %d: %w", a.installationID, err)
	}
	return nil
}

// Client returns a go-github client authenticated as the installation.
func (a *AppAuth) Client() *github.Client {
	return apiClient(a.transport, a.baseURL)
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
	installation, _, err := apiClient(a.apps, a.baseURL).Apps.GetInstallation(ctx, a.installationID)
	if err != nil {
		return AppRecord{}, fmt.Errorf("reading installation %d's own record: %w", a.installationID, err)
	}
	slug := installation.GetAppSlug()
	if slug == "" {
		return AppRecord{}, fmt.Errorf("installation %d's record carries no App slug", a.installationID)
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
