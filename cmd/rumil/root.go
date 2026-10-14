package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/config"
)

// newRootCmd builds the CLI. The config flag lives on the root command so every
// subcommand shares it.
func newRootCmd() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:           "rumil",
		Short:         "GitHub PR reviewer that learns your team's conventions",
		Version:       strings.TrimSpace(version + " (" + commit + ", " + date + ")"),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&configPath, "config", "c", "rumil.yaml", "path to the rumil configuration file")
	root.AddCommand(newReviewCmd(&configPath))
	root.AddCommand(newServeCmd(&configPath))
	root.AddCommand(newModCmd(&configPath))

	return root
}

// warnAboutModeration logs what a configuration means for the approval gate.
// Turning the gate off is a decision an operator may make, but it is a decision
// that comment-derived text reaches a review prompt without a maintainer
// reading it, so it is said out loud rather than left in a file. Every command
// that can record or resolve a rule says it: `review` neither records one nor
// resolves one, so it has nothing to report.
// serve also reports a missing admin token, because that is what decides
// whether the moderation surface is reachable at all.
func warnAboutModeration(cfg *config.Config, logger *slog.Logger, serve bool) {
	if cfg.Moderation.AcceptAll {
		logger.Warn("moderation.accept_all is on: a rule written in a comment reaches the review prompt without a maintainer approving it",
			"hint", "set moderation.accept_all to false and approve rules with `rumil mod approve`")
	}
	if serve && cfg.Moderation.AdminToken == "" {
		logger.Warn("moderation.admin_token is not set: the moderation UI and API refuse every request",
			"hint", "set moderation.admin_token to reach /ui/moderation and `rumil mod` over HTTP")
	}
}

// newLogger returns the structured JSON logger used for every job-scoped line.
func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))
}
