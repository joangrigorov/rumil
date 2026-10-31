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

	root.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"configuration file; without it, the first of ./rumil.yaml, ~/.config/rumil/config.yaml\n"+
			"or /etc/rumil/rumil.yaml is used, and "+config.EnvConfig+" names one instead")
	root.AddCommand(newReviewCmd(&configPath))
	root.AddCommand(newEvalCmd(&configPath))
	root.AddCommand(newServeCmd(&configPath))
	root.AddCommand(newSessionsCmd(&configPath))
	root.AddCommand(newFindingsCmd(&configPath))
	root.AddCommand(newModCmd(&configPath))
	root.AddCommand(newInitCmd(&configPath))
	root.AddCommand(newDoctorCmd(&configPath))

	return root
}

// loadConfig resolves the configuration file the way the flag documents, loads
// it, and hands back the path it read. Discovery means the operator does not
// always know which file was used, so every command that reads one says so.
func loadConfig(flag string) (*config.Config, string, error) {
	cfg, path, _, err := loadConfigWithRaw(flag)
	return cfg, path, err
}

// loadConfigWithRaw also returns the bytes the file held, which the server
// uses as the reload watcher's baseline.
func loadConfigWithRaw(flag string) (*config.Config, string, []byte, error) {
	path, err := config.Env(flag).Locate()
	if err != nil {
		return nil, "", nil, err
	}
	cfg, raw, err := config.LoadWithRaw(path)
	if err != nil {
		return nil, "", nil, err
	}
	return cfg, path, raw, nil
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
	if serve && len(cfg.Moderation.AdminCredentials()) == 0 {
		logger.Warn("no admin credential is set: the moderation UI, the deck and /metrics refuse every request",
			"hint", "set moderation.admin_token, or moderation.admin_tokens, to reach /ui/moderation and the deck")
	}
}

// newLogger returns the structured JSON logger used for every job-scoped line.
func newLogger(level string) *slog.Logger {
	logger, _ := newLevelLogger(level)
	return logger
}

// newLevelLogger returns the logger and the level it reads. The level is a
// variable so a reloaded app.log_level applies to loggers already handed out.
func newLevelLogger(level string) (*slog.Logger, *slog.LevelVar) {
	slogLevel := new(slog.LevelVar)
	slogLevel.Set(logLevel(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel})), slogLevel
}

// logLevel maps the validated app.log_level value to a slog level.
func logLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
