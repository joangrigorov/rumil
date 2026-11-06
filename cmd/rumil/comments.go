package main

import (
	"log/slog"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/moderation"
)

// memoryCommentEditor builds the GitHub surface that rewrites a rule's
// confirmation comment after a decision. A missing private key is not an error:
// the moderation surface still records decisions, and the comment simply keeps
// the status it was posted with.
func memoryCommentEditor(cfg *config.Config, logger *slog.Logger) moderation.CommentEditor {
	keyPEM, err := cfg.GitHub.ResolvePrivateKey()
	if err != nil {
		logger.Warn("moderation decisions will not update their GitHub comments", "error", err)
		return nil
	}
	auth, err := ghapp.NewAppAuth(cfg.GitHub, keyPEM, githubAPIBaseURL)
	if err != nil {
		logger.Warn("moderation decisions will not update their GitHub comments", "error", err)
		return nil
	}
	return ghapp.NewMemoryCommentEditor(auth)
}
