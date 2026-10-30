package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/doctor"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// doctorFakes wires the hermetic GitHub and model endpoints a healthy command
// line installation talks to, and returns the configuration file naming them.
func doctorFakes(t *testing.T) string {
	t.Helper()

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueText("OK")
	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	return writeConfig(t, llm.URL())
}

// A healthy installation exits zero, prints each check, and never prints the
// provider key.
func TestDoctorExitsZeroOnAHealthyInstance(t *testing.T) {
	configPath := doctorFakes(t)

	out, err := runRoot(t, "doctor", "--config", configPath, "--repo", "acme/payments")
	if err != nil {
		t.Fatalf("doctor returned error: %v (output: %s)", err, out)
	}
	for _, want := range []string{
		"config: " + configPath,
		"settings:",
		"checks:",
		"PASS config",
		"PASS github",
		"PASS chat",
		"WARN embedding",
		"SKIP database",
		"summary: 3 pass, 1 warn, 0 fail, 3 skip",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "test-key") {
		t.Errorf("the output carries the provider key:\n%s", out)
	}
}

// The JSON report is for scripts: it parses, and it carries the same checks.
func TestDoctorWritesJSON(t *testing.T) {
	configPath := doctorFakes(t)

	out, err := runRoot(t, "doctor", "--config", configPath, "--json")
	if err != nil {
		t.Fatalf("doctor --json returned error: %v (output: %s)", err, out)
	}

	var report doctor.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out)
	}
	if report.ConfigPath != configPath {
		t.Errorf("config_path = %q, want %q", report.ConfigPath, configPath)
	}
	if len(report.Checks) != 7 {
		t.Errorf("checks = %d, want 7", len(report.Checks))
	}
	if report.Failed() {
		t.Errorf("a healthy report says it failed: %+v", report.Checks)
	}
}

// A configured chat provider that cannot answer fails the run, and the exit
// carries the check-specific output.
func TestDoctorExitsNonZeroWhenChatFails(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	configPath := writeConfig(t, closed.URL)

	out, err := runRoot(t, "doctor", "--config", configPath)
	if err == nil {
		t.Fatalf("doctor returned no error for a down provider (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Errorf("error = %v, want it to name the failed checks", err)
	}
	if !strings.Contains(out, "FAIL chat") {
		t.Errorf("output does not carry the failed check:\n%s", out)
	}
	if strings.Contains(out, "test-key") {
		t.Errorf("the output carries the provider key:\n%s", out)
	}
}

// A value that cannot be a repository is refused before a request is made.
func TestDoctorRefusesAMalformedRepo(t *testing.T) {
	configPath := doctorFakes(t)

	out, err := runRoot(t, "doctor", "--config", configPath, "--repo", "acme")
	if err == nil {
		t.Fatalf("doctor accepted --repo acme (output: %s)", out)
	}
	if !strings.Contains(out, "FAIL github") {
		t.Errorf("output does not carry the failed check:\n%s", out)
	}
}
