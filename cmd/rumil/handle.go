package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/joangrigorov/rumil/internal/reviewer"
)

// checkHandle holds app.bot_handle against the App's own record before any work
// runs.
//
// The handle is the login Rumil answers to in comments and the login its own
// reviews are attributed to, and nothing else in the configuration is a copy of
// a fact GitHub already owns. A handle that is not the App's slug leaves the
// whole comment surface inert — a mention is not recognized, a review request
// is not recognized, and a review cannot tell its own threads from anyone
// else's — while the process looks healthy, so a mismatch stops the run. A
// record that could not be read is a network or credential failure that the
// run's own GitHub calls will surface, and does not.
func checkHandle(ctx context.Context, engine *reviewer.Engine, handle string, logger *slog.Logger) error {
	err := engine.VerifyHandle(ctx)
	switch {
	case err == nil:
		logger.Info("app.bot_handle matches the App's slug", "bot_handle", handle)
		return nil
	case errors.Is(err, reviewer.ErrHandleMismatch):
		return err
	default:
		logger.Warn("the App's own record could not be read, so app.bot_handle is unchecked", "error", err)
		return nil
	}
}
