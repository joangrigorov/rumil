// Package moderation is the surface that resolves a pending memory: the
// maintainer's decision to keep a rule, to edit it before keeping it, or to
// drop it. Both the CLI and the HTTP surface go through it, so a decision is
// one code path with one audit line whichever way it arrived
// (project_plan.md §6 Phase 5).
package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/joangrigorov/rumil/internal/store"
)

// Embedder turns a rule's text into the vector a search compares against. It is
// what makes an edited rule findable by the text a moderator approved, so the
// service refuses an edit when it has none.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ErrNoEmbedder reports an edit that cannot be made searchable. Memory is
// opt-in, so an operator can run a moderation surface with no embedding
// provider configured; approving the text as it was proposed still works.
var ErrNoEmbedder = errors.New("editing a rule needs a configured embedding provider")

// ErrUnknownStatus reports a status filter that names something a rule cannot
// be. It is checked before the query rather than left to the enum cast in the
// statement, so a typo is answered with what the three statuses are instead of
// a database error.
var ErrUnknownStatus = errors.New("status takes pending, approved or rejected")

// Service resolves pending memories for one installation.
type Service struct {
	store          *store.DB
	embedder       Embedder
	installationID int64
	logger         *slog.Logger
}

// NewService scopes the decisions to the installation Rumil serves. embedder
// may be nil, and only an edit needs it.
func NewService(db *store.DB, embedder Embedder, installationID int64, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: db, embedder: embedder, installationID: installationID, logger: logger}
}

// List returns the rules a moderator is looking at. The status filter is
// normalized here, which is the one place both the CLI and the HTTP surface
// pass through, so neither can hand the statement a value the enum will reject.
func (s *Service) List(ctx context.Context, filter store.MemoryFilter) ([]store.Memory, error) {
	status, err := normalizeStatus(filter.Status)
	if err != nil {
		return nil, err
	}
	filter.Status = status
	filter.InstallationID = s.installationID
	return s.store.ListMemories(ctx, filter)
}

// normalizeStatus turns a moderator's filter into one of the three statuses, or
// into no filter at all when nothing was asked for.
func normalizeStatus(status string) (string, error) {
	switch status = strings.ToUpper(strings.TrimSpace(status)); status {
	case "", "PENDING", "APPROVED", "REJECTED":
		return status, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownStatus, status)
	}
}

// Decision is one moderator's answer about one rule, as the CLI and the HTTP
// surface both express it.
type Decision struct {
	// RuleText is the rule as approved. Empty, or the text that was proposed,
	// keeps the proposed wording.
	RuleText string
	// PathGlob replaces the stored glob when it is not nil.
	PathGlob *string
	// Moderator is recorded as reviewed_by.
	Moderator string
}

// Approve keeps a rule, with the wording and the glob the moderator approved.
// An edit is re-embedded before the row is written, so the rule is found by the
// words that were approved rather than by the words that were proposed
// (T-030-03).
func (s *Service) Approve(ctx context.Context, id string, decision Decision) (store.Memory, error) {
	current, err := s.store.MemoryByID(ctx, s.installationID, id)
	if err != nil {
		return store.Memory{}, err
	}

	text := strings.TrimSpace(decision.RuleText)
	edited := text != "" && text != current.RuleText
	var embedding []float32
	if edited {
		if s.embedder == nil {
			return store.Memory{}, ErrNoEmbedder
		}
		embedding, err = s.embedder.Embed(ctx, text)
		if err != nil {
			return store.Memory{}, fmt.Errorf("embedding the edited rule: %w", err)
		}
	}

	approved, err := s.store.ApproveMemory(ctx, s.installationID, id, store.MemoryDecision{
		RuleText:   text,
		PathGlob:   decision.PathGlob,
		Embedding:  embedding,
		ReviewedBy: decision.Moderator,
	})
	if err != nil {
		return store.Memory{}, err
	}
	s.logger.Info("a rule was approved",
		"memory", approved.ID,
		"short_id", approved.ShortID(),
		"repo", approved.OrgName+"/"+approved.RepoName,
		"moderator", decision.Moderator,
		"edited", edited,
		"path_glob", approved.PathGlob,
	)
	return approved, nil
}

// Reject drops a rule from every read. The row stays for the record.
func (s *Service) Reject(ctx context.Context, id, moderator string) (store.Memory, error) {
	rejected, err := s.store.RejectMemory(ctx, s.installationID, id, moderator)
	if err != nil {
		return store.Memory{}, err
	}
	s.logger.Info("a rule was rejected",
		"memory", rejected.ID,
		"short_id", rejected.ShortID(),
		"repo", rejected.OrgName+"/"+rejected.RepoName,
		"moderator", moderator,
	)
	return rejected, nil
}
