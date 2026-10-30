package eval

import "strings"

// LineTolerance is how far an inline comment may sit from the line a fixture
// names. Models often anchor a finding to the statement above the one that is
// actually wrong, and treating that as a miss would measure line arithmetic
// rather than whether the defect was found.
const LineTolerance = 2

// Finding is one inline comment a review posted.
type Finding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// FixtureScore is how one fixture was reviewed.
type FixtureScore struct {
	Name string `json:"name"`
	// Expected and Matched are the counts; Missed names what was not found.
	Expected int           `json:"expected"`
	Matched  int           `json:"matched"`
	Missed   []Expectation `json:"missed"`
	// KnownFalsePositives are findings that matched the fixture's known-wrong
	// list; Unexpected are the rest of the findings nothing accounted for.
	KnownFalsePositives []Finding `json:"known_false_positives"`
	Unexpected          []Finding `json:"unexpected"`
	Findings            []Finding `json:"findings"`
	ReviewEvent         string    `json:"review_event"`
	// Error is why the fixture could not be reviewed, empty on a run.
	Error string `json:"error,omitempty"`
}

// Score matches the findings against the fixture's expectations. Each finding
// accounts for at most one expectation, and the first finding in the order the
// review posted them wins, so a review that repeats a finding does not score
// twice.
func Score(fixture Fixture, findings []Finding) FixtureScore {
	if findings == nil {
		findings = []Finding{}
	}
	score := FixtureScore{
		Name:                fixture.Name,
		Expected:            len(fixture.Expected),
		Findings:            findings,
		Missed:              []Expectation{},
		KnownFalsePositives: []Finding{},
		Unexpected:          []Finding{},
	}
	used := make([]bool, len(findings))

	for _, expected := range fixture.Expected {
		matched := false
		for i, finding := range findings {
			if used[i] || !matches(expected, finding) {
				continue
			}
			used[i] = true
			matched = true
			break
		}
		if !matched {
			score.Missed = append(score.Missed, expected)
			continue
		}
		score.Matched++
	}

	for i, finding := range findings {
		if used[i] {
			continue
		}
		if matchesAny(fixture.FalsePositives, finding) {
			score.KnownFalsePositives = append(score.KnownFalsePositives, finding)
			continue
		}
		score.Unexpected = append(score.Unexpected, finding)
	}
	return score
}

func matchesAny(expectations []Expectation, finding Finding) bool {
	for _, expected := range expectations {
		if matches(expected, finding) {
			return true
		}
	}
	return false
}

func matches(expected Expectation, finding Finding) bool {
	if finding.Path != expected.Path {
		return false
	}
	if abs(finding.Line-expected.Line) > LineTolerance {
		return false
	}
	if expected.Match == "" {
		return true
	}
	return strings.Contains(strings.ToLower(finding.Body), strings.ToLower(expected.Match))
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
