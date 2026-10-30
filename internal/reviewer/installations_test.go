package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const multiInstallationConfig = `
app:
  bot_handle: "@rumil"
github:
  app_id: 123456
  installation_id: 111
  installations:
    - id: 222
      name: "second"
  private_key_path: %q
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-model"
      api_key: "chat-key"
snapshot:
  enabled: false
`

func newMultiInstallationEngine(t *testing.T, gh *testutil.FakeGitHub) *Engine {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}
	doc := fmt.Sprintf(multiInstallationConfig, keyPath, gh.URL())
	cfg, err := config.Parse([]byte(doc), "installations_test.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v\n%s", err, doc)
	}

	engine, err := NewEngine(cfg, slog.New(slog.DiscardHandler), gh.URL(), nil)
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	return engine
}

func TestExplicitInstallationIsHeldAgainstTheConfiguration(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	engine := newMultiInstallationEngine(t, gh)

	got, err := engine.resolveInstallation(context.Background(), 222, gh.Owner, gh.Repo)
	if err != nil {
		t.Fatalf("resolveInstallation(222) returned error: %v", err)
	}
	if got != 222 {
		t.Errorf("installation = %d, want the requested 222", got)
	}

	if _, err := engine.resolveInstallation(context.Background(), 333, gh.Owner, gh.Repo); !errors.Is(err, ghapp.ErrInstallationNotAllowed) {
		t.Errorf("resolveInstallation(333) error = %v, want ErrInstallationNotAllowed", err)
	}
}

func TestZeroInstallationResolvesFromTheRepository(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	engine := newMultiInstallationEngine(t, gh)

	got, err := engine.resolveInstallation(context.Background(), 0, gh.Owner, gh.Repo)
	if err != nil {
		t.Fatalf("resolveInstallation(0) returned error: %v", err)
	}
	if got != 111 {
		t.Errorf("installation = %d, want the first configured installation that can read the repository", got)
	}

	log := strings.Join(gh.RequestLog(), "\n")
	if !strings.Contains(log, "GET /repos/acme/payments") {
		t.Errorf("requests = %s, want the repository probed to pick the installation", log)
	}
}

func TestSingleInstallationNeedsNoProbe(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	// The multi-installation fixture configures two; a single-installation
	// configuration is built here so the probe path can be asserted absent.
	doc := strings.Replace(multiInstallationConfig, `
  installations:
    - id: 222
      name: "second"`, "", 1)
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(doc, keyPath, gh.URL())), "single_test.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v", err)
	}
	single, err := NewEngine(cfg, slog.New(slog.DiscardHandler), gh.URL(), nil)
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}

	got, err := single.resolveInstallation(context.Background(), 0, gh.Owner, gh.Repo)
	if err != nil {
		t.Fatalf("resolveInstallation(0) returned error: %v", err)
	}
	if got != 111 {
		t.Errorf("installation = %d, want the only configured installation", got)
	}
	if log := strings.Join(gh.RequestLog(), "\n"); strings.Contains(log, "GET /repos/acme/payments") {
		t.Errorf("requests = %s, want no probe when one installation is configured", log)
	}
}
