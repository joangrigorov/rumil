package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/store"
)

// newFindingsCmd reports what happened to the inline comments Rumil posted, so
// a prompt or model change can be judged by measured outcomes instead of by
// rereading a few reviews.
func newFindingsCmd(configPath *string) *cobra.Command {
	var (
		repo   string
		days   int
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "findings",
		Short: "Report what happened to the findings Rumil posted",
		Long: "Count the inline comments Rumil posted and the ones that received a signal: a reply,\n" +
			"a reaction, a dismissed review, or a push after the round. The report is grouped by the\n" +
			"model that produced the review. A finding with no signal is not a failure; it is a\n" +
			"finding nobody has looked at yet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days < 0 {
				return fmt.Errorf("--days must be zero for all time or positive, got %d", days)
			}
			var orgName, repoName string
			if repo != "" {
				var err error
				orgName, repoName, err = splitRepo(repo)
				if err != nil {
					return err
				}
			}

			cfg, _, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			if cfg.Database.DSN == "" {
				return fmt.Errorf("database.dsn is required to read finding outcomes")
			}

			db, err := store.Open(cmd.Context(), cfg.Database.DSN, cfg.Database.AutoMigrate, cfg.Models.EmbeddingDimensions())
			if err != nil {
				return err
			}
			defer db.Close()

			filter := store.FindingFilter{OrgName: orgName, RepoName: repoName}
			if days > 0 {
				since := time.Now().AddDate(0, 0, -days)
				filter.Since = &since
			}
			stats, err := db.FindingStatsByModel(cmd.Context(), filter)
			if err != nil {
				return err
			}

			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(stats)
			}

			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "MODEL\tFINDINGS\tREPLIED\tRESOLVED\tREACTED\tDISMISSED\tHEAD MOVED")
			for _, row := range stats {
				model := row.Model
				if model == "" {
					model = "(unrecorded)"
				}
				_, _ = fmt.Fprintf(writer, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n",
					model, row.Findings, row.Replied, row.Resolved, row.Reacted, row.Dismissed, row.HeadMoved)
			}
			return writer.Flush()
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "limit to one repository, as owner/name")
	cmd.Flags().IntVar(&days, "days", 30, "only findings from the last N days (0 means all time)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}
