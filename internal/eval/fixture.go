// Package eval runs the reviewer against a fixture set without GitHub and
// scores the findings. It exists to compare prompt and model changes on a
// stable input rather than by reading a few live reviews.
package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults for the parts of a fixture every file would otherwise repeat.
const (
	DefaultOwner    = "acme"
	DefaultRepo     = "payments"
	DefaultNumber   = 42
	DefaultBaseSHA  = "1111111111111111111111111111111111111111"
	DefaultHeadSHA  = "2222222222222222222222222222222222222222"
	fixtureFileGlob = "*.yaml"
)

// Fixture is one pull request, the code it changes, and the findings a good
// review should produce.
type Fixture struct {
	// Name identifies the fixture in the report. It defaults to the file name
	// without the extension.
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Owner       string `yaml:"owner"`
	Repo        string `yaml:"repo"`
	Number      int    `yaml:"number"`
	Title       string `yaml:"title"`
	Body        string `yaml:"body"`
	BaseSHA     string `yaml:"base_sha"`
	HeadSHA     string `yaml:"head_sha"`
	// Diff is the annotated pull request diff, in the format GitHub serves.
	Diff string `yaml:"diff"`
	// Files is the full text of every file the diff touches, by path. A review
	// reads these through the file tools.
	Files map[string]string `yaml:"files"`
	// Expected lists the findings a good review should produce.
	Expected []Expectation `yaml:"expected"`
	// FalsePositives lists findings a review may produce that are known to be
	// wrong. They are counted separately from unexpected findings.
	FalsePositives []Expectation `yaml:"false_positives"`
}

// Expectation is one finding a fixture expects or rules out. A finding matches
// when the path is equal, the line is within a small tolerance, and the body
// contains Match, case-insensitively. An empty Match accepts any body.
type Expectation struct {
	Path  string `yaml:"path" json:"path"`
	Line  int    `yaml:"line" json:"line"`
	Match string `yaml:"match" json:"match"`
}

// LoadDir reads every fixture in dir, sorted by file name so a report is
// stable. The extension is the only filter; a directory holds fixtures and
// nothing else.
func LoadDir(dir string) ([]Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, fixtureFileGlob))
	if err != nil {
		return nil, fmt.Errorf("listing fixtures in %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no fixtures found in %s (want %s)", dir, fixtureFileGlob)
	}
	sort.Strings(paths)

	fixtures := make([]Fixture, 0, len(paths))
	for _, path := range paths {
		fixture, err := LoadFile(path)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures, nil
}

// LoadFile reads one fixture and applies the defaults.
func LoadFile(path string) (Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("reading fixture %s: %w", path, err)
	}

	var fixture Fixture
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		return Fixture{}, fmt.Errorf("parsing fixture %s: %w", path, err)
	}
	if fixture.Name == "" {
		fixture.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if fixture.Owner == "" {
		fixture.Owner = DefaultOwner
	}
	if fixture.Repo == "" {
		fixture.Repo = DefaultRepo
	}
	if fixture.Number == 0 {
		fixture.Number = DefaultNumber
	}
	if fixture.BaseSHA == "" {
		fixture.BaseSHA = DefaultBaseSHA
	}
	if fixture.HeadSHA == "" {
		fixture.HeadSHA = DefaultHeadSHA
	}
	if err := validate(fixture, path); err != nil {
		return Fixture{}, err
	}
	return fixture, nil
}

// validate refuses a fixture that cannot be run or scored, naming the file so a
// typo in one fixture does not look like a bad model run.
func validate(fixture Fixture, path string) error {
	var problems []string
	if strings.TrimSpace(fixture.Diff) == "" {
		problems = append(problems, "diff is empty")
	}
	if len(fixture.Files) == 0 {
		problems = append(problems, "files is empty")
	}
	if fixture.Number < 1 {
		problems = append(problems, "number must be positive")
	}
	for i, expected := range fixture.Expected {
		if err := expected.validate(); err != nil {
			problems = append(problems, fmt.Sprintf("expected[%d]: %v", i, err))
		}
	}
	for i, falsePositive := range fixture.FalsePositives {
		if err := falsePositive.validate(); err != nil {
			problems = append(problems, fmt.Sprintf("false_positives[%d]: %v", i, err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("fixture %s is invalid: %s", path, strings.Join(problems, "; "))
}

func (e Expectation) validate() error {
	var problems []string
	if strings.TrimSpace(e.Path) == "" {
		problems = append(problems, "path is required")
	}
	if e.Line < 1 {
		problems = append(problems, fmt.Sprintf("line must be positive for %s", e.Path))
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, ", "))
}
