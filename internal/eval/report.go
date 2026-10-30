package eval

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteTable writes the report as the fixed-width table an operator reads.
func WriteTable(w io.Writer, report Report) error {
	if _, err := fmt.Fprintf(w, "%-28s %8s %7s %6s %8s %10s  %s\n",
		"FIXTURE", "EXPECTED", "MATCHED", "MISSED", "KNOWN-FP", "UNEXPECTED", "STATUS"); err != nil {
		return err
	}
	for _, fixture := range report.Fixtures {
		status := "ok"
		if fixture.Error != "" {
			status = "error"
		}
		if _, err := fmt.Fprintf(w, "%-28s %8d %7d %6d %8d %10d  %s\n",
			fixture.Name, fixture.Expected, fixture.Matched, len(fixture.Missed),
			len(fixture.KnownFalsePositives), len(fixture.Unexpected), status); err != nil {
			return err
		}
	}

	totals := report.Totals()
	if _, err := fmt.Fprintf(w, "\n%d fixtures, %d expected, %d matched, %d missed, %d known false positives, %d unexpected, recall %.2f\n",
		totals.Fixtures, totals.Expected, totals.Matched, totals.Missed,
		totals.KnownFalsePositives, totals.Unexpected, totals.Recall); err != nil {
		return err
	}

	for _, fixture := range report.Fixtures {
		for _, missed := range fixture.Missed {
			if _, err := fmt.Fprintf(w, "  missed %s:%d %s\n", missed.Path, missed.Line, missed.Match); err != nil {
				return err
			}
		}
		for _, unexpected := range fixture.Unexpected {
			if _, err := fmt.Fprintf(w, "  unexpected %s:%d %s\n", unexpected.Path, unexpected.Line, oneLine(unexpected.Body)); err != nil {
				return err
			}
		}
		if fixture.Error != "" {
			if _, err := fmt.Fprintf(w, "  error %s: %s\n", fixture.Name, fixture.Error); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteJSON writes the same report as one JSON object, for a script that
// compares two runs.
func WriteJSON(w io.Writer, report Report) error {
	document := struct {
		Fixtures []FixtureScore `json:"fixtures"`
		Totals   Totals         `json:"totals"`
	}{Fixtures: report.Fixtures, Totals: report.Totals()}
	if document.Fixtures == nil {
		document.Fixtures = []FixtureScore{}
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// oneLine keeps a finding's first line, so a long comment does not break the
// table's shape.
func oneLine(body string) string {
	for i, r := range body {
		if r == '\n' || r == '\r' {
			return body[:i]
		}
	}
	return body
}
