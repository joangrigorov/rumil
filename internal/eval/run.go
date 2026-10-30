package eval

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// Runner reviews fixtures against the configured provider. GitHub is the
// in-process fake, so a fixture never touches a real repository and the review
// is submitted to the fake like it would be submitted to GitHub.
type Runner struct {
	Config *config.Config
	Logger *slog.Logger
	// Timeout bounds one fixture. Zero uses the configured worker timeout.
	Timeout time.Duration
}

// Report is every fixture's score, in the order given.
type Report struct {
	Fixtures []FixtureScore
}

// Totals aggregates the report.
type Totals struct {
	Fixtures            int     `json:"fixtures"`
	Failed              int     `json:"failed"`
	Expected            int     `json:"expected"`
	Matched             int     `json:"matched"`
	Missed              int     `json:"missed"`
	KnownFalsePositives int     `json:"known_false_positives"`
	Unexpected          int     `json:"unexpected"`
	Recall              float64 `json:"recall"`
}

// Totals sums the report. Recall is matched over expected findings, and 0 when
// the set expects nothing.
func (r Report) Totals() Totals {
	var totals Totals
	for _, fixture := range r.Fixtures {
		totals.Fixtures++
		if fixture.Error != "" {
			totals.Failed++
		}
		totals.Expected += fixture.Expected
		totals.Matched += fixture.Matched
		totals.Missed += len(fixture.Missed)
		totals.KnownFalsePositives += len(fixture.KnownFalsePositives)
		totals.Unexpected += len(fixture.Unexpected)
	}
	if totals.Expected > 0 {
		totals.Recall = float64(totals.Matched) / float64(totals.Expected)
	}
	return totals
}

// Run reviews every fixture in order. A fixture that fails is recorded and the
// set continues: one provider error should not throw away the rest of a run.
func (r Runner) Run(ctx context.Context, fixtures []Fixture) Report {
	logger := r.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	report := Report{Fixtures: make([]FixtureScore, 0, len(fixtures))}
	for _, fixture := range fixtures {
		score := r.runOne(ctx, logger, fixture)
		report.Fixtures = append(report.Fixtures, score)
	}
	return report
}

func (r Runner) runOne(ctx context.Context, logger *slog.Logger, fixture Fixture) FixtureScore {
	gh := testutil.NewFakeGitHubServer()
	defer gh.Server.Close()
	applyFixture(gh, fixture)

	engine, err := reviewer.NewEngine(r.Config, logger, gh.Server.URL, nil)
	if err != nil {
		return failedScore(fixture, fmt.Errorf("building the reviewer: %w", err))
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = r.Config.Worker.Timeout()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, reviewErr := engine.Review(runCtx, reviewer.Request{
		Owner:  fixture.Owner,
		Repo:   fixture.Repo,
		Number: fixture.Number,
	})

	score := Score(fixture, findingsOf(result))
	if result != nil {
		score.ReviewEvent = result.Event
	}
	if reviewErr != nil {
		score.Error = reviewErr.Error()
	}
	return score
}

// failedScore is a fixture that could not start: its expectations count as
// missed, and the error says why.
func failedScore(fixture Fixture, err error) FixtureScore {
	score := FixtureScore{Name: fixture.Name, Expected: len(fixture.Expected), Error: err.Error()}
	score.Missed = append(score.Missed, fixture.Expected...)
	return score
}

func findingsOf(result *reviewer.Result) []Finding {
	if result == nil {
		return nil
	}
	findings := make([]Finding, 0, len(result.Findings))
	for _, finding := range result.Findings {
		findings = append(findings, Finding{
			Path: finding.Path,
			Line: finding.Line,
			Body: finding.Body,
		})
	}
	return findings
}

// applyFixture points the fake at the fixture's pull request.
func applyFixture(gh *testutil.FakeGitHub, fixture Fixture) {
	gh.Owner = fixture.Owner
	gh.Repo = fixture.Repo
	gh.Number = fixture.Number
	gh.PRTitle = fixture.Title
	gh.PRBody = fixture.Body
	gh.BaseSHA = fixture.BaseSHA
	gh.HeadSHA = fixture.HeadSHA
	gh.Diff = fixture.Diff
	gh.Contents = fixture.Files
	gh.Files = fileEntries(fixture.Files)
}

func fileEntries(files map[string]string) []testutil.FileEntry {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]testutil.FileEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, testutil.FileEntry{Filename: name, Status: "modified"})
	}
	return entries
}
