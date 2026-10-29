package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/store"
)

// defaultRequester is who a continuation asked for on the command line is
// recorded against. The CLI has no identity to read, and this says so.
const defaultRequester = "cli"

// newSessionsCmd is the CLI surface for the sessions the server recorded.
func newSessionsCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Work with the review sessions the server recorded",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newSessionsContinueCmd(configPath))
	return cmd
}

// newSessionsContinueCmd implements `rumil sessions continue <session-id>`.
func newSessionsContinueCmd(configPath *string) *cobra.Command {
	var (
		turns       int
		requestedBy string
	)

	cmd := &cobra.Command{
		Use:   "continue <session-id>",
		Short: "Give a failed review session another attempt",
		Long: "Queue a second attempt at a review session that failed without submitting a review.\n" +
			"The new attempt reuses the session's stored tool results and partial summary, gets its own\n" +
			"turn budget, and is recorded as a session of its own. A session that already submitted a\n" +
			"review is refused. `rumil serve` runs the queued attempt.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if turns < 0 {
				return fmt.Errorf("--turns must be zero for the configured budget or positive, got %d", turns)
			}

			cfg, _, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			if cfg.Database.DSN == "" {
				return fmt.Errorf("database.dsn is required to read the review sessions")
			}

			db, err := store.Open(cmd.Context(), cfg.Database.DSN, cfg.Database.AutoMigrate, cfg.Models.EmbeddingDimensions())
			if err != nil {
				return err
			}
			defer db.Close()

			session, err := db.ReviewSessionByID(cmd.Context(), args[0])
			if errors.Is(err, store.ErrReviewSessionNotFound) {
				return fmt.Errorf("no review session %s", args[0])
			}
			if err != nil {
				return err
			}

			request, err := continuation.Queue(cmd.Context(), db, session, turns, requestedBy)
			switch {
			case errors.Is(err, continuation.ErrAlreadyQueued):
				return fmt.Errorf("session %s already has a continuation", session.ID)
			case errors.Is(err, store.ErrReviewAlreadySubmitted):
				return fmt.Errorf("session %s already submitted a review (%s), so it cannot be continued", session.ID, session.ReviewEvent)
			case errors.Is(err, store.ErrReviewNotFailed):
				return fmt.Errorf("session %s is %s; only a failed session can be continued", session.ID, session.Status)
			case err != nil:
				return err
			}

			budget := "the configured app.max_agent_turns"
			if request.Turns > 0 {
				budget = fmt.Sprintf("%d turns", request.Turns)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "continuation queued for %s (%s/%s#%d, %s, requested by %s)\n",
				session.ID, session.OrgName, session.RepoName, session.PRNumber, budget, request.RequestedBy)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "it runs when `rumil serve` next picks up the queue")
			return nil
		},
	}

	cmd.Flags().IntVar(&turns, "turns", 0, "agent steps for the new attempt (0 uses app.max_agent_turns)")
	cmd.Flags().StringVar(&requestedBy, "requested-by", defaultRequester, "who asked for the continuation, recorded on the new session")
	return cmd
}
