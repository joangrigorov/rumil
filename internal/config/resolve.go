package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvConfig names the environment variable that points at a configuration
// file, for the runs where a flag is awkward to pass: a container, a systemd
// unit, a CI job.
const EnvConfig = "RUMIL_CONFIG"

const (
	// localName is the only name searched in the working directory. A bare
	// config.yaml there would be ambiguous with every other tool's, so the
	// working directory has to carry the program's own name.
	localName = "rumil.yaml"
	// ownName is the name a configuration file takes inside a directory that
	// already belongs to Rumil, where config.yaml is unambiguous.
	ownName = "config.yaml"
	// defaultSystemDir is where a package installs the example configuration,
	// so it is where a machine-wide configuration belongs.
	defaultSystemDir = "/etc/rumil"
)

// Search is what path resolution reads. Env fills it in from the process; a
// test fills it in by hand.
type Search struct {
	// Flag is the --config/-c value, empty when the flag was not given.
	Flag string
	// EnvValue is $RUMIL_CONFIG, empty when it is unset.
	EnvValue string
	// WorkingDir is the directory the command was started in.
	WorkingDir string
	// HomeDir is the user's home directory, empty when there is none.
	HomeDir string
	// XDGConfigHome is $XDG_CONFIG_HOME, empty when it is unset.
	XDGConfigHome string
	// SystemDir is the machine-wide configuration directory. Env sets it to
	// /etc/rumil; a test points it at a directory it owns, so that what is
	// installed on the machine running the test cannot change the outcome.
	SystemDir string
}

// Env builds a Search from the process environment.
func Env(flag string) Search {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return Search{
		Flag:          flag,
		EnvValue:      os.Getenv(EnvConfig),
		WorkingDir:    wd,
		HomeDir:       home,
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		SystemDir:     defaultSystemDir,
	}
}

// Explicit is the file the operator named, through --config or $RUMIL_CONFIG.
// A named file is used as it is, whether or not it exists: the load reports a
// missing one, and discovery never runs behind the operator's back.
func (s Search) Explicit() string {
	if s.Flag != "" {
		return s.Flag
	}
	return s.EnvValue
}

// UserDir is the per-user configuration directory: $XDG_CONFIG_HOME/rumil, or
// ~/.config/rumil when the variable is unset. It is empty when neither is
// known.
func (s Search) UserDir() string {
	if s.XDGConfigHome != "" {
		return filepath.Join(s.XDGConfigHome, "rumil")
	}
	if s.HomeDir != "" {
		return filepath.Join(s.HomeDir, ".config", "rumil")
	}
	return ""
}

// Candidates lists every file discovery looks for, in the order it looks.
func (s Search) Candidates() []string {
	var out []string
	if s.WorkingDir != "" {
		out = append(out, filepath.Join(s.WorkingDir, localName))
	}
	if dir := s.UserDir(); dir != "" {
		out = append(out, filepath.Join(dir, ownName), filepath.Join(dir, localName))
	}
	if s.SystemDir != "" {
		out = append(out, filepath.Join(s.SystemDir, ownName), filepath.Join(s.SystemDir, localName))
	}
	return out
}

// Locate returns the configuration file to read. A file the operator named is
// returned as it is; otherwise the first candidate that exists wins, and an
// error naming every candidate is returned when none does.
func (s Search) Locate() (string, error) {
	if path := s.Explicit(); path != "" {
		return path, nil
	}
	for _, candidate := range s.Candidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no configuration file found; looked for %s, and --config or %s names one",
		strings.Join(s.Candidates(), ", "), EnvConfig)
}

// WriteTarget returns where a new configuration belongs: the file the operator
// named, otherwise the per-user directory, otherwise the working directory. It
// does not touch the filesystem.
func (s Search) WriteTarget() (string, error) {
	if path := s.Explicit(); path != "" {
		return path, nil
	}
	if dir := s.UserDir(); dir != "" {
		return filepath.Join(dir, ownName), nil
	}
	if s.WorkingDir != "" {
		return filepath.Join(s.WorkingDir, localName), nil
	}
	return "", errors.New("no home directory and no working directory: pass --config to say where the configuration belongs")
}
