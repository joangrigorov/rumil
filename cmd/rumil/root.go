package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
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

	return root
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
