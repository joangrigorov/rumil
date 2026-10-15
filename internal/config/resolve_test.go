package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testSearch returns a Search whose every directory is one the test owns, so
// that nothing installed on the machine running the test decides the outcome.
func testSearch(t *testing.T) Search {
	t.Helper()
	root := t.TempDir()
	return Search{
		WorkingDir:    filepath.Join(root, "work"),
		HomeDir:       filepath.Join(root, "home"),
		SystemDir:     filepath.Join(root, "etc", "rumil"),
		EnvValue:      "",
		XDGConfigHome: "",
	}
}

// write puts a file where a candidate would be, creating its directories.
func write(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("app:\n  log_level: debug\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// The order is the contract: the working directory first, so that what worked
// before discovery keeps working, then the per-user directory, then the
// machine-wide one, with config.yaml preferred inside a directory Rumil owns.
func TestCandidatesOrderIsTheDocumentedOne(t *testing.T) {
	s := testSearch(t)
	s.XDGConfigHome = filepath.Join(t.TempDir(), "xdg")

	want := []string{
		filepath.Join(s.WorkingDir, "rumil.yaml"),
		filepath.Join(s.XDGConfigHome, "rumil", "config.yaml"),
		filepath.Join(s.XDGConfigHome, "rumil", "rumil.yaml"),
		filepath.Join(s.SystemDir, "config.yaml"),
		filepath.Join(s.SystemDir, "rumil.yaml"),
	}

	got := s.Candidates()
	if len(got) != len(want) {
		t.Fatalf("Candidates() returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// XDG_CONFIG_HOME replaces ~/.config, and a Search without a home directory
// still has the other candidates.
func TestCandidatesFallBackToDotConfigWithoutXDG(t *testing.T) {
	s := testSearch(t)
	s.XDGConfigHome = ""

	got := s.Candidates()
	if want := filepath.Join(s.HomeDir, ".config", "rumil", "config.yaml"); got[1] != want {
		t.Errorf("second candidate = %s, want %s", got[1], want)
	}

	s.HomeDir = ""
	if got := s.Candidates(); len(got) != 3 {
		t.Errorf("with no home and no XDG dir, got %v, want the working directory and the system directory only", got)
	}
}

// The working directory wins because that is what the CLI did before discovery
// existed, and a repository's own file is the deliberate one.
func TestLocatePrefersTheWorkingDirectory(t *testing.T) {
	s := testSearch(t)
	s.XDGConfigHome = filepath.Join(t.TempDir(), "xdg")
	write(t, filepath.Join(s.WorkingDir, "rumil.yaml"))
	write(t, filepath.Join(s.SystemDir, "rumil.yaml"))

	got, err := s.Locate()
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if want := filepath.Join(s.WorkingDir, "rumil.yaml"); got != want {
		t.Errorf("Locate() = %s, want %s", got, want)
	}
}

// With no working-directory file, the per-user file is next, and config.yaml
// comes before rumil.yaml inside a directory that belongs to Rumil.
func TestLocatePrefersTheUserDirectoryOverTheSystemOne(t *testing.T) {
	s := testSearch(t)
	s.XDGConfigHome = filepath.Join(t.TempDir(), "xdg")
	write(t, filepath.Join(s.SystemDir, "config.yaml"))

	userDir := filepath.Join(s.XDGConfigHome, "rumil")
	write(t, filepath.Join(userDir, "rumil.yaml"))
	write(t, filepath.Join(userDir, "config.yaml"))

	got, err := s.Locate()
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if want := filepath.Join(userDir, "config.yaml"); got != want {
		t.Errorf("Locate() = %s, want %s", got, want)
	}
}

func TestLocateFallsBackToTheSystemDirectory(t *testing.T) {
	s := testSearch(t)
	write(t, filepath.Join(s.SystemDir, "rumil.yaml"))

	got, err := s.Locate()
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if want := filepath.Join(s.SystemDir, "rumil.yaml"); got != want {
		t.Errorf("Locate() = %s, want %s", got, want)
	}
}

// A directory named like a configuration file is not a configuration file.
func TestLocateIgnoresADirectoryNamedLikeTheFile(t *testing.T) {
	s := testSearch(t)
	if err := os.MkdirAll(filepath.Join(s.WorkingDir, "rumil.yaml"), 0o755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	write(t, filepath.Join(s.SystemDir, "rumil.yaml"))

	got, err := s.Locate()
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if want := filepath.Join(s.SystemDir, "rumil.yaml"); got != want {
		t.Errorf("Locate() = %s, want %s", got, want)
	}
}

// A named file is used as it is. Discovery never runs behind the operator's
// back: if the file is not there, the load says so.
func TestLocateUsesTheNamedFileWhetherOrNotItExists(t *testing.T) {
	s := testSearch(t)
	s.Flag = filepath.Join(t.TempDir(), "elsewhere.yaml")
	write(t, filepath.Join(s.WorkingDir, "rumil.yaml"))
	write(t, filepath.Join(s.SystemDir, "rumil.yaml"))

	got, err := s.Locate()
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got != s.Flag {
		t.Errorf("Locate() = %s, want the named file %s even though it does not exist", got, s.Flag)
	}

	s.Flag = ""
	s.EnvValue = "/tmp/from-the-environment.yaml"
	if got, err = s.Locate(); err != nil || got != s.EnvValue {
		t.Errorf("Locate() = %s, %v, want %s from %s", got, err, s.EnvValue, EnvConfig)
	}
}

// The flag beats the environment variable.
func TestExplicitPrefersTheFlagOverTheEnvironment(t *testing.T) {
	s := Search{Flag: "/from/the/flag.yaml", EnvValue: "/from/the/environment.yaml"}
	if got := s.Explicit(); got != s.Flag {
		t.Errorf("Explicit() = %s, want %s", got, s.Flag)
	}
}

// Finding nothing has to be actionable, so the error names every path that was
// tried and the flag that would bypass the search.
func TestLocateNamesEveryCandidateWhenNothingIsFound(t *testing.T) {
	s := testSearch(t)
	s.XDGConfigHome = filepath.Join(t.TempDir(), "xdg")

	_, err := s.Locate()
	if err == nil {
		t.Fatal("Locate() found a configuration file in an empty tree")
	}
	for _, candidate := range s.Candidates() {
		if !strings.Contains(err.Error(), candidate) {
			t.Errorf("error does not name %s: %v", candidate, err)
		}
	}
	if !strings.Contains(err.Error(), "--config") || !strings.Contains(err.Error(), EnvConfig) {
		t.Errorf("error does not say how to name a file instead: %v", err)
	}
}

// A new configuration belongs beside the ones discovery reads: the named file
// when there is one, otherwise the per-user directory.
func TestWriteTarget(t *testing.T) {
	t.Run("the named file wins", func(t *testing.T) {
		s := testSearch(t)
		s.Flag = "/somewhere/else.yaml"
		got, err := s.WriteTarget()
		if err != nil || got != s.Flag {
			t.Errorf("WriteTarget() = %s, %v, want %s", got, err, s.Flag)
		}
	})

	t.Run("otherwise the user directory", func(t *testing.T) {
		s := testSearch(t)
		s.XDGConfigHome = filepath.Join(t.TempDir(), "xdg")
		got, err := s.WriteTarget()
		if err != nil {
			t.Fatalf("WriteTarget() error = %v", err)
		}
		if want := filepath.Join(s.XDGConfigHome, "rumil", "config.yaml"); got != want {
			t.Errorf("WriteTarget() = %s, want %s", got, want)
		}
	})

	t.Run("without a home, the working directory", func(t *testing.T) {
		s := testSearch(t)
		s.HomeDir = ""
		got, err := s.WriteTarget()
		if err != nil {
			t.Fatalf("WriteTarget() error = %v", err)
		}
		if want := filepath.Join(s.WorkingDir, "rumil.yaml"); got != want {
			t.Errorf("WriteTarget() = %s, want %s", got, want)
		}
	})

	t.Run("with neither, an error that says what to pass", func(t *testing.T) {
		_, err := Search{}.WriteTarget()
		if err == nil || !strings.Contains(err.Error(), "--config") {
			t.Errorf("WriteTarget() error = %v, want one naming --config", err)
		}
	})
}
