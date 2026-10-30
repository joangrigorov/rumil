package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/doctor"
)

// newDoctorCmd implements `rumil doctor`.
func newDoctorCmd(configPath *string) *cobra.Command {
	var (
		asJSON       bool
		repo         string
		snapshotRepo string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the configuration against the services it depends on",
		Long: "Load and validate the configuration, then check each service it names: Postgres and its schema,\n" +
			"the GitHub App credentials, one chat completion, the embedding provider when memory is on, and\n" +
			"the credentials the webhook server needs. Nothing is written and no review is started.\n\n" +
			"Pass --repo owner/name to confirm the installation can see the repository, and\n" +
			"pass --snapshot owner/name to download and extract that repository's default branch into a\n" +
			"temporary directory, which is removed afterwards.\n\n" +
			"The command exits non-zero when a configured component fails. A warning, such as memory being\n" +
			"off, does not fail the run. Secrets are never printed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, path, err := loadConfig(*configPath)
			if err != nil {
				return err
			}

			report := doctor.Run(cmd.Context(), doctor.Options{
				Config:           cfg,
				ConfigPath:       path,
				GitHubAPIBaseURL: githubAPIBaseURL,
				Repo:             repo,
				SnapshotRepo:     snapshotRepo,
			})

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else {
				writeDoctorReport(out, report)
			}

			if report.Failed() {
				return errors.New("one or more checks failed")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "write the report as JSON")
	cmd.Flags().StringVar(&repo, "repo", "", "confirm the installation can read this repository as owner/name")
	cmd.Flags().StringVar(&snapshotRepo, "snapshot", "", "download and extract this repository as owner/name")
	return cmd
}

// writeDoctorReport prints the effective settings and each check. The columns
// keep a long check list readable, and the summary is the line a script's human
// reads first.
func writeDoctorReport(out io.Writer, report doctor.Report) {
	if report.ConfigPath != "" {
		_, _ = fmt.Fprintf(out, "config: %s\n", report.ConfigPath)
	}
	if len(report.Settings) > 0 {
		_, _ = fmt.Fprintln(out, "settings:")
		for _, setting := range report.Settings {
			_, _ = fmt.Fprintf(out, "  %s: %s\n", setting.Name, setting.Value)
		}
	}

	_, _ = fmt.Fprintln(out, "checks:")
	for _, check := range report.Checks {
		_, _ = fmt.Fprintf(out, "  %-4s %-10s %s\n",
			strings.ToUpper(string(check.Status)), check.Name, check.Detail)
	}

	counts := report.Counts()
	_, _ = fmt.Fprintf(out, "summary: %d pass, %d warn, %d fail, %d skip\n",
		counts[doctor.Pass], counts[doctor.Warn], counts[doctor.Fail], counts[doctor.Skip])
}
