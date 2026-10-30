package reviewer

import (
	"context"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// Acknowledge reacts to a review request Rumil has accepted. A review request
// carries no comment, so the pull request is the surface the reaction goes on.
//
// Callers treat a failure as a warning: a reaction that could not be posted is
// not a review that cannot run.
func (e *Engine) Acknowledge(ctx context.Context, req Request) error {
	installationID, err := e.resolveInstallation(ctx, req.InstallationID, req.Owner, req.Repo)
	if err != nil {
		return err
	}
	authClient, err := e.auth.ClientFor(installationID)
	if err != nil {
		return err
	}
	client := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)
	return client.ReactToPullRequest(ctx, ghapp.ReactionAcknowledged)
}

// AcknowledgeCommand reacts to the comment that carried a command, which is
// where the person who typed it is looking for an answer.
func (e *Engine) AcknowledgeCommand(ctx context.Context, req RememberRequest) error {
	if req.CommentID == 0 {
		return nil
	}
	installationID, err := e.resolveInstallation(ctx, req.InstallationID, req.Owner, req.Repo)
	if err != nil {
		return err
	}
	authClient, err := e.auth.ClientFor(installationID)
	if err != nil {
		return err
	}
	client := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)
	return client.ReactToComment(ctx, req.CommentID, !req.Conversation, ghapp.ReactionAcknowledged)
}
