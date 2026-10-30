package store

import (
	"context"
	"fmt"
	"time"
)

// Finding is one inline comment Rumil posted. The comment id is what later
// feedback arrives on, which is why it is the row's natural key.
type Finding struct {
	SessionID      string
	InstallationID int64
	OrgName        string
	RepoName       string
	PRNumber       int
	ReviewID       int64
	CommentID      int64
	Path           string
	Line           int
	Body           string
}

// FindingStats is the outcome report for one model: how many findings it
// posted and how many received a signal.
type FindingStats struct {
	Model     string `json:"model"`
	Findings  int    `json:"findings"`
	Replied   int    `json:"replied"`
	Resolved  int    `json:"resolved"`
	Reacted   int    `json:"reacted"`
	Dismissed int    `json:"dismissed"`
	HeadMoved int    `json:"head_moved"`
}

// FindingFilter narrows the report. An empty org and repository mean every
// repository, and a nil Since means every finding on record.
type FindingFilter struct {
	OrgName  string
	RepoName string
	Since    *time.Time
}

// RecordFindings stores the inline comments a review posted. A comment id the
// table already has is left alone, so a retried delivery cannot duplicate a
// finding.
func (d *DB) RecordFindings(ctx context.Context, findings []Finding) error {
	for _, finding := range findings {
		if finding.CommentID == 0 {
			continue
		}
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO review_findings (session_id, installation_id, org_name, repo_name, pr_number,
				review_id, comment_id, path, line, body)
			VALUES (nullif($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (comment_id) DO NOTHING`,
			finding.SessionID, finding.InstallationID, finding.OrgName, finding.RepoName, finding.PRNumber,
			finding.ReviewID, finding.CommentID, finding.Path, finding.Line, finding.Body); err != nil {
			return fmt.Errorf("recording finding %d: %w", finding.CommentID, err)
		}
	}
	return nil
}

// MarkFindingReplied records that somebody answered a finding. The first reply
// is the one kept, because later replies are usually the conversation that
// follows it.
func (d *DB) MarkFindingReplied(ctx context.Context, commentID int64) error {
	if commentID == 0 {
		return nil
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE review_findings
		SET replied_at = coalesce(replied_at, now())
		WHERE comment_id = $1`, commentID); err != nil {
		return fmt.Errorf("marking finding %d replied: %w", commentID, err)
	}
	return nil
}

// MarkFindingsResolved records the findings whose threads Rumil closed after
// verifying the code no longer contains them.
func (d *DB) MarkFindingsResolved(ctx context.Context, commentIDs []int64) error {
	for _, commentID := range commentIDs {
		if commentID == 0 {
			continue
		}
		if _, err := d.pool.Exec(ctx, `
			UPDATE review_findings
			SET resolved_at = coalesce(resolved_at, now())
			WHERE comment_id = $1`, commentID); err != nil {
			return fmt.Errorf("marking finding %d resolved: %w", commentID, err)
		}
	}
	return nil
}

// MarkFindingReaction records the last reaction on a finding. Reactions are
// free-form, so the content is stored as GitHub named it.
func (d *DB) MarkFindingReaction(ctx context.Context, commentID int64, reaction string) error {
	if commentID == 0 || reaction == "" {
		return nil
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE review_findings
		SET reaction = $2
		WHERE comment_id = $1`, commentID, reaction); err != nil {
		return fmt.Errorf("marking finding %d reaction: %w", commentID, err)
	}
	return nil
}

// MarkReviewFindingsDismissed records that a review was dismissed, which is the
// strongest signal that its findings were not accepted.
func (d *DB) MarkReviewFindingsDismissed(ctx context.Context, reviewID int64) error {
	if reviewID == 0 {
		return nil
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE review_findings
		SET dismissed_at = coalesce(dismissed_at, now())
		WHERE review_id = $1`, reviewID); err != nil {
		return fmt.Errorf("marking review %d findings dismissed: %w", reviewID, err)
	}
	return nil
}

// MarkPRFindingsHeadMoved records that the pull request moved after a round.
// It is a proxy for "the code changed", not a verdict on the finding.
func (d *DB) MarkPRFindingsHeadMoved(ctx context.Context, orgName, repoName string, number int) error {
	if orgName == "" || repoName == "" || number <= 0 {
		return nil
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE review_findings
		SET head_moved_at = coalesce(head_moved_at, now())
		WHERE org_name = $1 AND repo_name = $2 AND pr_number = $3`, orgName, repoName, number); err != nil {
		return fmt.Errorf("marking %s/%s#%d findings moved: %w", orgName, repoName, number, err)
	}
	return nil
}

// FindingStatsByModel returns the outcome report, grouped by the model that
// produced the review. Findings from a session that was not recorded are
// grouped under the empty model.
func (d *DB) FindingStatsByModel(ctx context.Context, filter FindingFilter) ([]FindingStats, error) {
	var since *time.Time
	if filter.Since != nil {
		since = filter.Since
	}
	rows, err := d.pool.Query(ctx, `
		SELECT coalesce(s.model, '') AS model,
		       count(*),
		       count(*) FILTER (WHERE f.replied_at IS NOT NULL),
		       count(*) FILTER (WHERE f.resolved_at IS NOT NULL),
		       count(*) FILTER (WHERE f.reaction IS NOT NULL),
		       count(*) FILTER (WHERE f.dismissed_at IS NOT NULL),
		       count(*) FILTER (WHERE f.head_moved_at IS NOT NULL)
		FROM review_findings f
		LEFT JOIN review_sessions s ON s.id = f.session_id
		WHERE ($1 = '' OR f.org_name = $1)
		  AND ($2 = '' OR f.repo_name = $2)
		  AND ($3::timestamptz IS NULL OR f.created_at >= $3)
		GROUP BY 1
		ORDER BY 1`, filter.OrgName, filter.RepoName, since)
	if err != nil {
		return nil, fmt.Errorf("reading finding outcomes: %w", err)
	}
	defer rows.Close()

	var stats []FindingStats
	for rows.Next() {
		var row FindingStats
		if err := rows.Scan(&row.Model, &row.Findings, &row.Replied, &row.Resolved, &row.Reacted, &row.Dismissed, &row.HeadMoved); err != nil {
			return nil, fmt.Errorf("scanning finding outcomes: %w", err)
		}
		stats = append(stats, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading finding outcomes: %w", err)
	}
	return stats, nil
}
