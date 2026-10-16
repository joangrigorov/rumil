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

// Slug reads the App's own record and returns the slug GitHub knows it by,
// which is the login people mention in comments and the login the App's own
// reviews are attributed to. GET /app is app-authenticated rather than
// installation-authenticated, so this is the one read that works before an
// installation token is minted.
func (a *AppAuth) Slug(ctx context.Context) (string, error) {
	app, _, err := apiClient(a.apps, a.baseURL).Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("reading the App's own record: %w", err)
	}
	slug := app.GetSlug()
	if slug == "" {
		return "", errors.New("the App's own record carries no slug")
	}
	return slug, nil
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
