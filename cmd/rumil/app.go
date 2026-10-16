package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/joangrigorov/rumil/internal/reviewer"
)

// checkApp holds the configuration against the App's own record before any work
// runs.
//
// `app.bot_handle` is the login Rumil answers to in comments and the login its
// own reviews are attributed to, and nothing else in the configuration is a copy
// of a fact GitHub already owns. A handle that is not the App's slug leaves the
// whole comment surface inert — a mention is not recognized, a review request is
// not recognized, and a review cannot tell its own threads from anyone else's —
// while the process looks healthy, so a mismatch stops the run. A record that
// could not be read is a network or credential failure that the run's own GitHub
// calls will surface, and does not.
//
// The rest of the record is reported rather than enforced: GitHub refuses to let
// an App close a review thread unless the App holds Contents: Read and write,
// and that refusal arrives at the end of a round that has already been paid for.
func checkApp(ctx context.Context, engine *reviewer.Engine, handle string, logger *slog.Logger) error {
	warnings, err := engine.CheckApp(ctx)
	switch {
	case errors.Is(err, reviewer.ErrHandleMismatch):
		return err
	case err != nil:
		logger.Warn("the App's own record could not be read, so app.bot_handle is unchecked", "error", err)
		return nil
	}

	logger.Info("app.bot_handle matches the App's slug", "bot_handle", handle)
	for _, warning := range warnings {
		logger.Warn(warning)
	}
	return nil
}
