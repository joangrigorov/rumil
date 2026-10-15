package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MemoryScope values of the memory_scope enum.
const (
	ScopeRepo = "REPO"
	ScopeOrg  = "ORG"
)

// MemoryStatus values of the memory_status enum.
const (
	MemoryPending  = "PENDING"
	MemoryApproved = "APPROVED"
	MemoryRejected = "REJECTED"
)

// Bounds on the rules one search returns (project_plan.md §5.2).
const (
	MemoryLimitDefault = 5
	MemoryLimitMax     = 15
)

// nearDuplicateSimilarity is the cosine similarity at which two proposals count
// as the same rule, so the second is answered with the first rather than stored
// twice (T-020-04).
const nearDuplicateSimilarity = 0.92

// pathOverFetch is how many candidates extra the search asks Postgres for when
// a file path is set. The glob is a row value, so Postgres cannot apply it to
// the ordering: without slack, a glob-filtered search would come back short of
// the caller's limit.
const pathOverFetch = 5

// Memory is one row of memories. The embedding is never read back — a review
// needs the rules and their distance, not the vectors.
type Memory struct {
	ID             string
	InstallationID int64
	OrgName        string
	RepoName       string
	Scope          string
	Status         string
	PathGlob       string
	RuleText       string
	Rationale      string
	SourcePR       int
	SourceComment  string
	RequestedBy    string
	ReviewedBy     string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ShortID is the leading UUID group, which is what a confirmation reply names
// so a maintainer can paste it into a moderation command.
func (m Memory) ShortID() string {
	if i := strings.IndexByte(m.ID, '-'); i > 0 {
		return m.ID[:i]
	}
	return m.ID
}

// Proposal is a memory a comment asked Rumil to record. The columns the model
// does not supply are filled by the caller from the triggering comment.
type Proposal struct {
	InstallationID int64
	OrgName        string
	RepoName       string
	Scope          string
	RuleText       string
	Rationale      string
	PathGlob       string
	SourcePR       int
	SourceComment  string
	RequestedBy    string
	// Approved stores the row APPROVED instead of PENDING. It is true only when
	// moderation.accept_all is on, which is off by default.
	Approved  bool
	Embedding []float32
}

// MemoryMatch is one candidate rule with how close it came to the query.
// Similarity is the cosine similarity, so higher is closer.
type MemoryMatch struct {
	ID         string
	Scope      string
	RepoName   string
	PathGlob   string
	RuleText   string
	Rationale  string
	Similarity float64
}

// MemoryQuery selects the approved rules that may apply to a review.
type MemoryQuery struct {
	InstallationID int64
	OrgName        string
	RepoName       string
	Embedding      []float32
	// FilePath, when set, drops every rule whose glob does not match it. An
	// empty path keeps every candidate. A NULL or empty glob applies to every
	// path.
	FilePath string
	// RepoScopeBoost is added to a REPO-scoped rule's similarity when the
	// candidates are ranked, so a project-specific rule outranks a general
	// organization rule that came back at a similar distance
	// (project_plan.md §6 Phase 6). The returned Similarity stays the distance
	// the embedding actually has: the boost decides the order, not the number.
	// Zero ranks by similarity alone.
	RepoScopeBoost float64
	// Limit is clamped to [MemoryLimitDefault, MemoryLimitMax].
	Limit int
}

const memoryColumns = `id, installation_id, org_name, coalesce(repo_name, ''), scope, status,
	coalesce(path_glob, ''), rule_text, coalesce(rationale, ''), source_pr, source_comment,
	requested_by, coalesce(reviewed_by, ''), created_at, updated_at`

// ProposeMemory stores a proposal and reports whether it inserted a row. A rule
// already stored that sits within nearDuplicateSimilarity of this one is
// returned instead of a second row, counting PENDING rows too so a proposal
// awaiting moderation is not duplicated either.
//
// What a proposal is compared against is what the review of this repository
// would already retrieve: the repository's own rules and its organization's. A
// repository proposal that restates either of them would put the same rule in
// front of the reviewer twice, so the existing row is what the person is
// answered with rather than a second copy. An organization proposal is a
// widening instead, so the only rule it can duplicate is another organization
// rule: a repository rule says nothing about the rest of the organization, and
// asking for the wider rule is a request to record rather than a duplicate to
// report.
//
// The threshold is a similarity, not a meaning, so a repository exception
// worded closely enough to its organization's rule to sit above it is read as
// the same rule and not stored separately. That is the pair the ranking boost
// exists to order, and it is the pair whose wording is a whole sentence apart:
// at this distance the two rules say the same thing, and the confirmation tells
// the person which rule answered them and what it reaches.
//
// The duplicate check and the insert are separate statements. Two proposals
// arriving at once can therefore both pass the check; the cost is a duplicate
// row for a maintainer to reject, which is not worth a lock on the tenant.
func (d *DB) ProposeMemory(ctx context.Context, p Proposal) (memory Memory, inserted bool, err error) {
	existing, err := d.nearDuplicate(ctx, p)
	if err != nil {
		return Memory{}, false, err
	}
	if existing != nil {
		return *existing, false, nil
	}

	status := MemoryPending
	if p.Approved {
		status = MemoryApproved
	}
	rows, err := d.pool.Query(ctx, `
		INSERT INTO memories (installation_id, org_name, repo_name, scope, status, path_glob,
		                      rule_text, rationale, source_pr, source_comment, requested_by, embedding)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, nullif($8, ''), $9, $10, $11, $12::vector)
		RETURNING `+memoryColumns,
		p.InstallationID, p.OrgName, p.RepoName, p.Scope, status, p.PathGlob,
		p.RuleText, p.Rationale, p.SourcePR, p.SourceComment, p.RequestedBy, vectorLiteral(p.Embedding))
	if err != nil {
		return Memory{}, false, fmt.Errorf("inserting a memory for %s/%s: %w", p.OrgName, p.RepoName, err)
	}
	defer rows.Close()

	stored, err := scanMemory(rows)
	if err != nil {
		return Memory{}, false, err
	}
	return *stored, true, nil
}

func (d *DB) nearDuplicate(ctx context.Context, p Proposal) (*Memory, error) {
	scope := p.Scope
	if scope == "" {
		scope = ScopeRepo
	}
	rows, err := d.pool.Query(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE installation_id = $1
		  AND org_name = $2
		  AND status IN ('PENDING', 'APPROVED')
		  -- A repository proposal repeats what already applies where it was
		  -- written, which is this repository's own rules and the
		  -- organization's, the same candidates a search for this repository
		  -- ranks. An organization proposal widens the rule, so only a rule
		  -- that already reaches the organization can be the same rule.
		  AND CASE WHEN $3::memory_scope = 'ORG'
		           THEN scope = 'ORG'
		           ELSE scope = 'ORG' OR repo_name = $4
		      END
		  AND 1 - (embedding <=> $5::vector) > $6
		ORDER BY embedding <=> $5::vector
		LIMIT 1`,
		p.InstallationID, p.OrgName, scope, p.RepoName, vectorLiteral(p.Embedding), nearDuplicateSimilarity)
	if err != nil {
		return nil, fmt.Errorf("looking for a near-duplicate memory: %w", err)
	}
	defer rows.Close()

	found, err := scanMemory(rows)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return found, nil
}

// SearchMemories returns the approved rules closest to the query, best first.
//
// The tenant is the installation and the organization together: repository
// names are unique per owner, so matching on repo_name alone would let one
// customer's rule reach another's review (T-020-05). ORG-scoped rules are
// candidates everywhere in their organization; REPO-scoped ones only in their
// repository.
func (d *DB) SearchMemories(ctx context.Context, query MemoryQuery) ([]MemoryMatch, error) {
	limit := query.Limit
	switch {
	case limit <= 0:
		limit = MemoryLimitDefault
	case limit > MemoryLimitMax:
		limit = MemoryLimitMax
	}
	fetch := limit
	if query.FilePath != "" {
		fetch = limit * pathOverFetch
	}

	// The ranking is the query's, so the repository boost is applied before the
	// limit rather than over a page that was already cut: an organization rule
	// that scores highest, and a repository rule just behind it, must both be in
	// the five the caller gets. The score is computed in the subquery because
	// Postgres allows an output name in ORDER BY but not inside an expression.
	rows, err := d.pool.Query(ctx, `
		SELECT id, scope, repo_name, path_glob, rule_text, rationale, similarity
		FROM (
		    SELECT id, scope, coalesce(repo_name, '') AS repo_name,
		           coalesce(path_glob, '') AS path_glob, rule_text,
		           coalesce(rationale, '') AS rationale,
		           1 - (embedding <=> $1::vector) AS similarity,
		           (1 - (embedding <=> $1::vector))
		               + CASE WHEN scope = 'REPO' THEN $6::double precision ELSE 0::double precision END AS score
		    FROM memories
		    WHERE installation_id = $2
		      AND org_name = $3
		      AND status = 'APPROVED'
		      AND (
		          (scope = 'REPO' AND repo_name = $4)
		          OR scope = 'ORG'
		      )
		) AS ranked
		ORDER BY score DESC, id
		LIMIT $5`,
		vectorLiteral(query.Embedding), query.InstallationID, query.OrgName, query.RepoName, fetch, query.RepoScopeBoost)
	if err != nil {
		return nil, fmt.Errorf("searching memories: %w", err)
	}
	defer rows.Close()

	matches := make([]MemoryMatch, 0, limit)
	for rows.Next() {
		var m MemoryMatch
		if err := rows.Scan(&m.ID, &m.Scope, &m.RepoName, &m.PathGlob, &m.RuleText, &m.Rationale, &m.Similarity); err != nil {
			return nil, fmt.Errorf("scanning a memory match: %w", err)
		}
		if query.FilePath != "" && m.PathGlob != "" && !globMatch(m.PathGlob, query.FilePath) {
			continue
		}
		if len(matches) == limit {
			break
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("searching memories: %w", err)
	}
	return matches, nil
}

// globMatch reports whether a file path matches the glob a rule carries. The
// syntax is the one people write in review comments: `**` crosses path
// segments, `*` and `?` stay inside one, and everything else is literal.
func globMatch(glob, filePath string) bool {
	return regexp.MustCompile(globPattern(glob)).MatchString(filePath)
}

// globPattern translates a glob into an anchored regular expression. Every
// other byte goes through QuoteMeta, so the result always compiles.
func globPattern(glob string) string {
	var b strings.Builder
	b.WriteString(`\A`)
	literal := 0
	flush := func(end int) {
		if end > literal {
			b.WriteString(regexp.QuoteMeta(glob[literal:end]))
		}
	}
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			flush(i)
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				b.WriteString(`.*`)
			} else {
				b.WriteString(`[^/]*`)
			}
			literal = i + 1
		case '?':
			flush(i)
			b.WriteString(`[^/]`)
			literal = i + 1
		}
	}
	flush(len(glob))
	b.WriteString(`\z`)
	return b.String()
}

func scanMemory(rows pgx.Rows) (*Memory, error) {
	if !rows.Next() {
		return nil, rows.Err()
	}

	var m Memory
	if err := rows.Scan(
		&m.ID, &m.InstallationID, &m.OrgName, &m.RepoName, &m.Scope, &m.Status,
		&m.PathGlob, &m.RuleText, &m.Rationale, &m.SourcePR, &m.SourceComment,
		&m.RequestedBy, &m.ReviewedBy, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning a memory row: %w", err)
	}
	return &m, nil
}

// vectorLiteral renders an embedding in the text form pgvector parses. The
// width is the embedder's business: it refuses a provider that does not return
// config.EmbeddingDimensions floats, so the column is never handed the wrong
// size.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
