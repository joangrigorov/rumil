package ghapp

import (
	"context"
	"errors"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func multiInstallationConfig() config.GitHub {
	return config.GitHub{
		AppID:          123,
		InstallationID: 11,
		Installations: []config.Installation{
			{ID: 22, Name: "second"},
		},
	}
}

func TestVerifyAllMintsOneTokenPerInstallation(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	auth, err := NewAppAuth(multiInstallationConfig(), testutil.TestPrivateKeyPEM(t), gh.URL())
	if err != nil {
		t.Fatalf("NewAppAuth returned error: %v", err)
	}

	if err := auth.VerifyAll(context.Background()); err != nil {
		t.Fatalf("VerifyAll returned error: %v", err)
	}
	if got := gh.MintCount(); got != 2 {
		t.Errorf("mints = %d, want one per configured installation", got)
	}

	// Each installation's transport caches its own token.
	if err := auth.VerifyAll(context.Background()); err != nil {
		t.Fatalf("the second VerifyAll returned error: %v", err)
	}
	if got := gh.MintCount(); got != 2 {
		t.Errorf("mints after a second VerifyAll = %d, want the cached tokens reused", got)
	}
}

func TestUnconfiguredInstallationIsRefused(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	auth, err := NewAppAuth(multiInstallationConfig(), testutil.TestPrivateKeyPEM(t), gh.URL())
	if err != nil {
		t.Fatalf("NewAppAuth returned error: %v", err)
	}

	if err := auth.VerifyInstallation(context.Background(), 99); !errors.Is(err, ErrInstallationNotAllowed) {
		t.Fatalf("VerifyInstallation(99) error = %v, want ErrInstallationNotAllowed", err)
	}
	if _, err := auth.ClientFor(99); !errors.Is(err, ErrInstallationNotAllowed) {
		t.Fatalf("ClientFor(99) error = %v, want ErrInstallationNotAllowed", err)
	}
	if _, err := auth.RecordFor(context.Background(), 99); !errors.Is(err, ErrInstallationNotAllowed) {
		t.Fatalf("RecordFor(99) error = %v, want ErrInstallationNotAllowed", err)
	}
	if got := gh.MintCount(); got != 0 {
		t.Errorf("mints = %d, want no token for an installation the configuration does not list", got)
	}
}

func TestPrimaryInstallationIsTheLegacyID(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	auth, err := NewAppAuth(multiInstallationConfig(), testutil.TestPrivateKeyPEM(t), gh.URL())
	if err != nil {
		t.Fatalf("NewAppAuth returned error: %v", err)
	}

	if err := auth.VerifyInstallation(context.Background(), 0); err != nil {
		t.Fatalf("VerifyInstallation(0) returned error: %v", err)
	}
	if got := gh.MintCount(); got != 1 {
		t.Fatalf("mints = %d, want the primary installation's token", got)
	}

	// Minting for the primary again reuses the cached transport.
	if err := auth.Verify(context.Background()); err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if got := gh.MintCount(); got != 1 {
		t.Errorf("mints = %d, want the cached token reused", got)
	}
}
