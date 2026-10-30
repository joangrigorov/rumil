package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// githubAPIBaseURL overrides the GitHub API endpoint. It is empty in
// production and exists as a seam for hermetic CLI tests.
var githubAPIBaseURL string

// newReviewCmd implements `rumil review --repo owner/name --pr <number>`.
func newReviewCmd(configPath *string) *cobra.Command {
	var (
		repo string
		pr   int
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review one pull request and submit the review",
		Long: "Review one pull request with the configured model and submit the review to GitHub.\n" +
			"The review is read-only: the only write is the review itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			owner, name, err := splitRepo(repo)
			if err != nil {
				return err
			}
			if pr < 1 {
				return fmt.Errorf("--pr must be a positive pull request number, got %d", pr)
			}

			cfg, path, err := loadConfig(*configPath)
			if err != nil {
				return err
			}

			logger := newLogger(cfg.App.LogLevel).With(
				"config", path,
				"installation_id", cfg.GitHub.InstallationID,
				"repo", owner+"/"+name,
				"pr", pr,
			)

			// The store is what pre-retrieval reads recorded rules from, so the
			// one-shot review opens it only when the operator configured a
			// database (project_plan.md §3 rule 6).
			var db *store.DB
			if cfg.Database.DSN != "" {
				db, err = store.Open(cmd.Context(), cfg.Database.DSN, cfg.Database.AutoMigrate, cfg.Models.EmbeddingDimensions())
				if err != nil {
					return err
				}
				defer db.Close()
			}

			engine, err := reviewer.NewEngine(cfg, logger, githubAPIBaseURL, db)
			if err != nil {
				return err
			}

			// A handle that is not the App's slug costs this run its own review
			// history and its ability to close its own threads, so the run stops
			// before it spends a model call on it.
			if err := checkApp(cmd.Context(), engine, cfg.App.BotHandle, logger); err != nil {
				return err
			}

			result, err := engine.Review(cmd.Context(), reviewer.Request{Owner: owner, Repo: name, Number: pr})
			if err != nil {
				return err
			}

			if !result.ReviewSubmitted {
				// The engine turns this into ErrNoReviewSubmitted, so reaching
				// here means the two paths disagree. Fail rather than print a
				// success line for a review that was never submitted.
				return fmt.Errorf("%w for %s/%s#%d", reviewer.ErrNoReviewSubmitted, owner, name, pr)
			}

			logger.Info("review submitted",
				"event", result.Event,
				"fallback", result.Fallback,
				"moved_to_summary", result.MovedToSummary,
				"skipped_duplicates", result.SkippedDuplicates,
				"tool_calls", result.ToolCalls,
			)
			if result.Usage != nil {
				var cost any
				if result.Usage.CostUSD != nil {
					cost = *result.Usage.CostUSD
				}
				logger.Info("review usage",
					"input_tokens", result.Usage.InputTokens,
					"output_tokens", result.Usage.OutputTokens,
					"cache_read_tokens", result.Usage.CacheReadTokens,
					"cache_creation_tokens", result.Usage.CacheCreationTokens,
					"total_tokens", result.Usage.TotalTokens(),
					"estimated_cost_usd", cost,
				)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "review submitted to %s/%s#%d as %s\n", owner, name, pr, result.Event)
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "target repository as owner/name (required)")
	cmd.Flags().IntVar(&pr, "pr", 0, "pull request number (required)")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("pr")

	return cmd
}

// splitRepo validates the --repo flag.
func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("--repo must be owner/name, got %q", repo)
	}
	return parts[0], parts[1], nil
}
