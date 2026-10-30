package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func TestSplitRepo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		repo      string
		wantOrg   string
		wantRepo  string
		wantError bool
	}{
		{"owner and name", "acme/payments", "acme", "payments", false},
		{"no separator", "payments", "", "", true},
		{"no owner", "/payments", "", "", true},
		{"no name", "acme/", "", "", true},
		{"too many parts", "acme/payments/extra", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgName, repoName, err := splitRepo(tc.repo)
			if (err != nil) != tc.wantError {
				t.Fatalf("splitRepo(%q) error = %v, want error %v", tc.repo, err, tc.wantError)
			}
			if orgName != tc.wantOrg || repoName != tc.wantRepo {
				t.Errorf("splitRepo(%q) = %q, %q", tc.repo, orgName, repoName)
			}
		})
	}
}

// The report reads the stored findings and prints them as JSON, grouped by the
// model that produced the review.
func TestFindingsReportPrintsJSON(t *testing.T) {
	dsn := testutil.TestDatabaseURL(t, "rumil_findings_cmd")
	db := openSessionsStore(t, dsn)
	ctx := context.Background()

	session, err := db.CreateReviewSession(ctx, store.NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating a session: %v", err)
	}
	if err := db.RecordFindings(ctx, []store.Finding{{
		SessionID:      session.ID,
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		ReviewID:       1,
		CommentID:      9001,
		Path:           "main.go",
		Line:           12,
		Body:           "a finding",
	}}); err != nil {
		t.Fatalf("recording a finding: %v", err)
	}

	configPath := writeConfigWithDatabase(t, dsn)
	cmd := newFindingsCmd(&configPath)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--repo", "acme/payments", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("findings command returned error: %v", err)
	}

	var stats []store.FindingStats
	if err := json.Unmarshal(output.Bytes(), &stats); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, output.String())
	}
	if len(stats) != 1 {
		t.Fatalf("stats = %+v, want one model", stats)
	}
	if stats[0].Model != "test-model" || stats[0].Findings != 1 {
		t.Errorf("stats = %+v, want the one finding under test-model", stats[0])
	}
}
