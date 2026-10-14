package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrMemoryNotFound reports a moderation command that names a row which is not
// there. A mistyped id and another tenant's id look the same from the outside
// on purpose: every query here is scoped to the installation, so a moderator
// cannot learn whether a row exists somewhere else.
var ErrMemoryNotFound = errors.New("memory not found")

// ErrAmbiguousMemory reports a reference that matches more than one row of the
// tenant. A full id never does; a truncated one could, and the moderator is
// asked to be more specific rather than given whichever row sorted first.
var ErrAmbiguousMemory = errors.New("memory id is ambiguous")

// memoryRefPattern is what a moderation command may name: a UUID, or the
// leading group of one, which is the short id a confirmation reply puts in the
// pull request so a maintainer can paste it. Validating the shape first is what
// keeps a stray `%` out of the LIKE pattern.
var memoryRefPattern = regexp.MustCompile(`^[0-9a-fA-F][0-9a-fA-F-]{3,35}$`)

// Bounds on a moderation listing.
const (
	MemoryListDefault = 50
	MemoryListMax     = 200
)

// MemoryFilter selects the rows a moderation view lists. InstallationID is
// required, so a listing never crosses tenants.
type MemoryFilter struct {
	InstallationID int64
	// Status filters on memory_status when it is set, and lists every status
	// otherwise.
	Status string
	// OrgName and RepoName narrow by repository when they are set, and match
	// every repository otherwise.
	OrgName  string
	RepoName string
	// Limit is clamped to [MemoryListDefault, MemoryListMax].
	Limit int
}

// ListMemories returns the tenant's rules, newest first, which is the order a
// moderation queue is worked in.
func (d *DB) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	if filter.InstallationID == 0 {
		return nil, fmt.Errorf("listing memories needs an installation")
	}
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = MemoryListDefault
	case limit > MemoryListMax:
		limit = MemoryListMax
	}

	// An empty filter is passed as nullif(...) rather than built into the
	// statement, so the query stays one statement the planner can cache. A cast
	// of the empty string would raise on the enum, which is why the nullif is
	// inside the cast rather than around it.
	rows, err := d.pool.Query(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE installation_id = $1
		  AND status = coalesce(nullif($2, '')::memory_status, status)
		  AND org_name = coalesce(nullif($3, ''), org_name)
		  AND coalesce(repo_name, '') = coalesce(nullif($4, ''), coalesce(repo_name, ''))
		ORDER BY created_at DESC, id DESC
		LIMIT $5`,
		filter.InstallationID, filter.Status, filter.OrgName, filter.RepoName, limit)
	if err != nil {
		return nil, fmt.Errorf("listing memories: %w", err)
	}
	defer rows.Close()

	memories := make([]Memory, 0, limit)
	for {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("listing memories: %w", err)
		}
		if memory == nil {
			break
		}
		memories = append(memories, *memory)
	}
	return memories, nil
}

// MemoryByID returns one row of this tenant. The reference may be the full id
// or the leading group the confirmation reply names, because that is what a
// maintainer has in front of them on the pull request.
func (d *DB) MemoryByID(ctx context.Context, installationID int64, id string) (Memory, error) {
	if installationID == 0 {
		return Memory{}, fmt.Errorf("reading a memory needs an installation")
	}
	reference := strings.ToLower(strings.TrimSpace(id))
	if !memoryRefPattern.MatchString(reference) {
		return Memory{}, fmt.Errorf("%w: %q is not a memory id", ErrMemoryNotFound, id)
	}

	// Two rows is enough to tell an ambiguous reference from a unique one, and
	// the ordering makes which one comes back deterministic.
	rows, err := d.pool.Query(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE installation_id = $1 AND id::text LIKE $2 || '%'
		ORDER BY id
		LIMIT 2`, installationID, reference)
	if err != nil {
		return Memory{}, fmt.Errorf("reading memory %s: %w", id, err)
	}
	defer rows.Close()

	memory, err := scanMemory(rows)
	if err != nil {
		return Memory{}, fmt.Errorf("reading memory %s: %w", id, err)
	}
	if memory == nil {
		return Memory{}, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
	}
	second, err := scanMemory(rows)
	if err != nil {
		return Memory{}, fmt.Errorf("reading memory %s: %w", id, err)
	}
	if second != nil {
		return Memory{}, fmt.Errorf("%w: %s matches %s and %s", ErrAmbiguousMemory, id, memory.ID, second.ID)
	}
	return *memory, nil
}

// MemoryDecision is what a moderator decided about one rule.
type MemoryDecision struct {
	// RuleText is the rule as approved. Empty, or the text that was proposed,
	// keeps the stored row as it is.
	RuleText string
	// PathGlob replaces the stored glob when it is not nil, so a moderator can
	// widen a rule to the whole repository by clearing it.
	PathGlob *string
	// Embedding is the vector for RuleText, and is required when RuleText is an
	// edit: the store does not call an embedding provider, and keeping the old
	// vector would leave a rule that no longer matches what a search looks for
	// (T-030-03).
	Embedding []float32
	// ReviewedBy records who decided.
	ReviewedBy string
}

// ApproveMemory records a maintainer's decision to keep a rule, with the
// wording they approved and the glob they gave it.
func (d *DB) ApproveMemory(ctx context.Context, installationID int64, id string, decision MemoryDecision) (Memory, error) {
	return d.reviewMemory(ctx, installationID, id, MemoryApproved, decision)
}

// RejectMemory records a maintainer's decision to drop a rule. The row stays:
// what the team already asked for is worth keeping, and a rejected rule is
// inert everywhere a rule is read (T-030-04).
func (d *DB) RejectMemory(ctx context.Context, installationID int64, id, reviewedBy string) (Memory, error) {
	return d.reviewMemory(ctx, installationID, id, MemoryRejected, MemoryDecision{ReviewedBy: reviewedBy})
}

func (d *DB) reviewMemory(ctx context.Context, installationID int64, id, status string, decision MemoryDecision) (Memory, error) {
	if installationID == 0 {
		return Memory{}, fmt.Errorf("reviewing a memory needs an installation")
	}
	// The reference is resolved first, so a moderator can act on the short id a
	// confirmation reply named, and an ambiguous or unknown one is refused
	// before anything is written.
	resolved, err := d.MemoryByID(ctx, installationID, id)
	if err != nil {
		return Memory{}, err
	}

	// The text, the glob and the vector are only replaced when the decision
	// carries them, so a rejection cannot erase what was proposed.
	rows, err := d.pool.Query(ctx, `
		UPDATE memories
		SET status = $3::memory_status,
		    rule_text = coalesce(nullif($4, ''), rule_text),
		    path_glob = coalesce($5, path_glob),
		    embedding = coalesce($6::vector, embedding),
		    reviewed_by = nullif($7, ''),
		    updated_at = now()
		WHERE installation_id = $1 AND id = $2
		RETURNING `+memoryColumns,
		installationID, resolved.ID, status, decision.RuleText, decision.PathGlob,
		vectorLiteralOrNull(decision.Embedding), decision.ReviewedBy)
	if err != nil {
		return Memory{}, fmt.Errorf("updating memory %s: %w", id, err)
	}
	defer rows.Close()

	memory, err := scanMemory(rows)
	if err != nil {
		return Memory{}, fmt.Errorf("updating memory %s: %w", id, err)
	}
	if memory == nil {
		return Memory{}, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
	}
	return *memory, nil
}

// vectorLiteralOrNull renders an embedding for the statement, and lets the
// column keep its value when there is none.
func vectorLiteralOrNull(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	return vectorLiteral(v)
}
