package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
)

// initAnswers is what a full run of `rumil init` is fed, in the order the
// questions are asked. The last line answers the confirmation.
const initAnswers = "acme-rumil\n" +
	"123456\n" +
	"98765432\n" +
	"GITHUB_APP_PRIVATE_KEY_PATH\n" +
	"https://api.deepseek.com/v1\n" +
	"deepseek-chat\n" +
	"DEEPSEEK_API_KEY\n" +
	"text-embedding-3-small\n" +
	"https://api.openai.com/v1\n" +
	"OPENAI_API_KEY\n" +
	"a-webhook-secret-value\n" +
	"DATABASE_URL\n" +
	"RUMIL_ADMIN_TOKEN\n" +
	"y\n"

// tempHome points HOME at a directory the test owns and clears
// XDG_CONFIG_HOME, so the file lands where the test looks for it whatever the
// developer's environment says.
func tempHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// runInitCmd runs the command with the answers as its standard input, and
// captures everything it prints.
func runInitCmd(t *testing.T, in string, args ...string) (string, error) {
	t.Helper()

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(in))
	root.SetArgs(append([]string{"init"}, args...))

	err := root.Execute()
	return out.String(), err
}

// The point of the command: the file lands where discovery looks for it when
// --config is not given, and the runtime loads it.
func TestInitWritesTheFileDiscoveryFinds(t *testing.T) {
	home := tempHome(t)

	out, err := runInitCmd(t, initAnswers)
	if err != nil {
		t.Fatalf("init returned error: %v (output: %s)", err, out)
	}

	path := filepath.Join(home, ".config", "rumil", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("init did not write %s: %v (output: %s)", path, err, out)
	}
	// The file can hold a webhook secret and a moderation token.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600", path, perm)
	}
	if !strings.Contains(out, "wrote "+path) {
		t.Errorf("the command does not say where the file went:\n%s", out)
	}

	// And discovery finds it, without a flag.
	found, err := config.Env("").Locate()
	if err != nil {
		t.Fatalf("discovery does not find the file init wrote: %v", err)
	}
	if found != path {
		t.Errorf("discovery found %s, want %s", found, path)
	}

	// The file is one the runtime reads, once the variables it names are set.
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github_app.pem")
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("DATABASE_URL", "postgres://rumil:rumil@db:5432/rumil?sslmode=disable")
	t.Setenv("RUMIL_ADMIN_TOKEN", "a-token")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file init wrote does not load: %v", err)
	}
	if cfg.App.BotHandle != "acme-rumil" || cfg.GitHub.AppID != 123456 || cfg.GitHub.InstallationID != 98765432 {
		t.Errorf("configuration = %+v, want the answers", cfg)
	}
	if cfg.GitHub.WebhookSecret != "a-webhook-secret-value" {
		t.Errorf("webhook_secret = %q", cfg.GitHub.WebhookSecret)
	}
	if len(cfg.Models.Providers) != 2 {
		t.Fatalf("providers = %d, want the chat and embedding providers", len(cfg.Models.Providers))
	}
	if cfg.Models.Providers[1].Dimensions != config.EmbeddingDimensions {
		t.Errorf("embedding provider = %+v", cfg.Models.Providers[1])
	}

	// The secrets the answers named are variables in the file, not values.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file back: %v", err)
	}
	for _, placeholder := range []string{"${DEEPSEEK_API_KEY}", "${OPENAI_API_KEY}", "${DATABASE_URL}", "${RUMIL_ADMIN_TOKEN}"} {
		if !strings.Contains(string(raw), placeholder) {
			t.Errorf("the file does not carry %s:\n%s", placeholder, raw)
		}
	}
}

// An existing file is left alone unless --force says otherwise, and the refusal
// comes before the first question.
func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	home := tempHome(t)

	path := filepath.Join(home, ".config", "rumil", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("app:\n  bot_handle: mine\n"), 0o600); err != nil {
		t.Fatalf("writing the existing file: %v", err)
	}

	out, err := runInitCmd(t, initAnswers)
	if err == nil {
		t.Fatalf("init overwrote an existing file: %s", out)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, want it to name --force", err)
	}
	if strings.Contains(out, "The GitHub App") {
		t.Errorf("the questions ran before the refusal:\n%s", out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "app:\n  bot_handle: mine\n" {
		t.Errorf("the existing file changed:\n%s", raw)
	}

	out, err = runInitCmd(t, initAnswers, "--force")
	if err != nil {
		t.Fatalf("init --force returned error: %v (output: %s)", err, out)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "acme-rumil") {
		t.Errorf("--force did not replace the file:\n%s", raw)
	}
}

// --print writes the file to stdout and touches nothing, which is what a
// container needs and what a reader wants before committing to a path.
func TestInitPrintWritesToStdoutAndNoFile(t *testing.T) {
	home := tempHome(t)

	out, err := runInitCmd(t, initAnswers, "--print")
	if err != nil {
		t.Fatalf("init --print returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "# Rumil configuration.") || !strings.Contains(out, "bot_handle: acme-rumil") {
		t.Errorf("--print did not write the configuration to stdout:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "rumil", "config.yaml")); err == nil {
		t.Error("--print wrote a file")
	}
	// The confirmation is about writing a file, so --print does not ask it.
	if strings.Contains(out, "Write "+filepath.Join(home, ".config", "rumil", "config.yaml")+"?") {
		t.Errorf("--print asked whether to write the file:\n%s", out)
	}
}

// Answering no leaves nothing behind rather than an empty file.
func TestInitWritesNothingWhenTheConfirmationIsDeclined(t *testing.T) {
	home := tempHome(t)

	in := strings.Replace(initAnswers, "\ny\n", "\nn\n", 1)
	out, err := runInitCmd(t, in)
	if err != nil {
		t.Fatalf("init returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("output does not say nothing was written:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "rumil", "config.yaml")); err == nil {
		t.Error("a file was written after the confirmation was declined")
	}
}

// The path the operator names is the path that is written.
func TestInitWritesToTheNamedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "elsewhere", "rumil.yaml")

	out, err := runInitCmd(t, initAnswers, "--config", path)
	if err != nil {
		t.Fatalf("init --config returned error: %v (output: %s)", err, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("init did not write the named path: %v (output: %s)", err, out)
	}
}

// An answer that cannot make a working configuration stops the command instead
// of writing a file the runtime will refuse.
func TestInitRefusesAnAnswerThatCannotWork(t *testing.T) {
	home := tempHome(t)

	broken := strings.Replace(initAnswers, "https://api.deepseek.com/v1\n", "not-a-url\nnot-a-url\n", 1)
	out, err := runInitCmd(t, broken)
	if err == nil {
		t.Fatalf("init wrote a file from an unusable answer:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".config", "rumil", "config.yaml")); statErr == nil {
		t.Error("a file was written although the answers were refused")
	}
}
