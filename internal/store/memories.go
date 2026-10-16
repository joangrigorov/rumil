package store

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// MemoryAuthority values of the memory_authority enum.
//
// AuthorityContext is what every rule is stored as, and it is what a proposal
// can only ever be: recorded context, which the repository's own guidelines
// outrank. AuthorityDirective is set by a maintainer at the approval gate and
// puts one rule above those guidelines for the case it names, which is why no
// proposal path can write it: a rule reaches the reviewer with authority over
// the repository's own policy only after a person promoted it
// (project_plan.md §10 item 5).
const (
	AuthorityContext   = "CONTEXT"
	AuthorityDirective = "DIRECTIVE"
)

// Bounds on the rules one search returns (project_plan.md §5.2).
const (
	MemoryLimitDefault = 5
	MemoryLimitMax     = 15
)

// nearDuplicateSimilarity is the cosine similarity above which an existing rule
// is a candidate for being the rule a proposal restates, so the second is
// answered with the first rather than stored twice (T-020-04).
//
// It is a candidate filter and not the decision. A similarity is a distance on
// one embedding model's scale, and models differ in how far apart they place
// two texts that share nothing but their subject: a review of a proposal found
// that a rule about link targets, worded by the synthesis session as "In the
// snip HTTP API (internal/httpapi), a link target must be stored exactly as
// NormalizeTarget returns it" scored 0.9208 against "In the snip HTTP API
// (internal/httpapi), every request-handling route that mutates state must be
// registered behind the requireAdmin middleware" — over a threshold that was
// the whole decision, so a rule nobody had recorded was answered as already
// covered and never stored. On that model, rules of one repository sit between
// 0.78 and 0.89 from one another whatever they say, so no single threshold
// separates the two pairs.
const nearDuplicateSimilarity = 0.92

// duplicateCandidates is how many rules above nearDuplicateSimilarity the word
// check looks at. A candidate that the words refuse does not end the search,
// because the closest rule to a proposal is not always the one it restates.
const duplicateCandidates = 3

// duplicateWordOverlap is the share of words two rule texts have in common
// before the second is answered as the first. The words are what carries the
// decision, because they are what a compressed embedding scale loses: the two
// rules above share the four words of their opening clause and nothing else,
// which is 0.15, while a rule restated in a reviewer's own words keeps the
// words of the decision itself — "clock skew in the JWT validator" against "the
// JWT validator allowing 60s of clock skew" is 0.62.
//
// The bias is deliberate. A rule that is stored twice costs a maintainer one
// rejection at the gate, and the reply to the second person names the rule it
// looks like. A rule that is answered as a duplicate it is not is stored
// nowhere, and nobody sees it go.
const duplicateWordOverlap = 0.45

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
	// Authority is CONTEXT for every rule a comment proposed, and DIRECTIVE
	// only once a maintainer promoted it at the approval gate.
	Authority     string
	Status        string
	PathGlob      string
	RuleText      string
	Rationale     string
	SourcePR      int
	SourceComment string
	RequestedBy   string
	ReviewedBy    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ShortID is the leading UUID group, which is what a confirmation reply names
// so a maintainer can paste it into a moderation command.
func (m Memory) ShortID() string { return shortID(m.ID) }

// ShortID is the same short form for a match, which is what a log line names so
// the rules a review retrieved can be looked up afterwards.
func (m MemoryMatch) ShortID() string { return shortID(m.ID) }

// shortID is the leading UUID group of an id.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
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
	Approved bool
	// There is deliberately no Authority field. A proposal is stored as
	// AuthorityContext whatever it is, and the statement below names that value
	// rather than leaning on the column default, so the one path that can raise
	// a rule above the repository's guidelines stays the approval gate. An
	// operator who turns moderation off has skipped the gate, so a rule that
	// arrives that way is context like any other.
	Embedding []float32
}

// MemoryMatch is one candidate rule with how close it came to the query.
// Similarity is the cosine similarity, so higher is closer.
type MemoryMatch struct {
	ID        string
	Scope     string
	RepoName  string
	PathGlob  string
	RuleText  string
	Rationale string
	// Authority is the rule's memory_authority, which is what lets the reader
	// tell a rule a maintainer promoted above the repository's guidelines from
	// recorded context.
	Authority  string
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

const memoryColumns = `id, installation_id, org_name, coalesce(repo_name, ''), scope, authority, status,
	coalesce(path_glob, ''), rule_text, coalesce(rationale, ''), source_pr, source_comment,
	requested_by, coalesce(reviewed_by, ''), created_at, updated_at`

// ProposeMemory stores a proposal and reports whether it inserted a row. A rule
// already stored that sits within nearDuplicateSimilarity of this one and shares
// the words of the decision with it is returned instead of a second row,
// counting PENDING rows too so a proposal awaiting moderation is not duplicated
// either.
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
// Both signals have to agree: the similarity puts a rule in the running and the
// words of the two rules decide, because a distance alone merges rules that
// share nothing but the clause they open with.
//
// The trade the words make is deliberate. Two rules that say one thing in
// different words are stored twice, which costs a maintainer a rejection at the
// gate, while a rule answered as a duplicate it is not is stored nowhere and
// nobody sees it go.
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
		INSERT INTO memories (installation_id, org_name, repo_name, scope, authority, status, path_glob,
		                      rule_text, rationale, source_pr, source_comment, requested_by, embedding)
		VALUES ($1, $2, $3, $4, 'CONTEXT', $5, nullif($6, ''), $7, nullif($8, ''), $9, $10, $11, $12::vector)
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
		LIMIT $7`,
		p.InstallationID, p.OrgName, scope, p.RepoName, vectorLiteral(p.Embedding), nearDuplicateSimilarity, duplicateCandidates)
	if err != nil {
		return nil, fmt.Errorf("looking for a near-duplicate memory: %w", err)
	}
	defer rows.Close()

	for {
		found, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		if found == nil {
			// No candidate left: the proposal is a rule of its own.
			return nil, nil
		}
		// The similarity only put this rule in the running. What decides is
		// whether the two say the same thing, which on a compressed embedding
		// scale the distance cannot tell (see duplicateWordOverlap).
		if wordOverlap(found.RuleText, p.RuleText) >= duplicateWordOverlap {
			return found, nil
		}
	}
}

// ruleWords returns the words a rule is made of: lowercased, without
// punctuation, and without the words that say nothing about which decision the
// rule records. Numbers are kept, because "60s" and "7 characters" are part of
// the decision.
func ruleWords(text string) map[string]struct{} {
	words := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if _, stop := ruleStopWords[field]; stop {
			continue
		}
		words[field] = struct{}{}
	}
	return words
}

// wordOverlap is the share of words two rule texts have in common, counted over
// the words both of them use at most once, so a rule that repeats a word is not
// closer to another for it.
func wordOverlap(a, b string) float64 {
	first, second := ruleWords(a), ruleWords(b)
	if len(first) == 0 || len(second) == 0 {
		return 0
	}
	shared := 0
	for word := range first {
		if _, ok := second[word]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(first)+len(second)-shared)
}

// ruleStopWords are the words a rule shares with every other rule, so counting
// them would make two rules look alike for being written in English.
var ruleStopWords = map[string]struct{}{
	"a": {}, "all": {}, "also": {}, "an": {}, "and": {}, "any": {}, "are": {},
	"as": {}, "at": {}, "be": {}, "because": {}, "but": {}, "by": {}, "can": {},
	"do": {}, "does": {}, "every": {}, "for": {}, "from": {}, "has": {}, "have": {},
	"in": {}, "into": {}, "is": {}, "it": {}, "its": {}, "may": {}, "must": {},
	"no": {}, "not": {}, "of": {}, "on": {}, "one": {}, "only": {}, "or": {},
	"other": {}, "so": {}, "that": {}, "the": {}, "their": {}, "them": {},
	"then": {}, "there": {}, "these": {}, "they": {}, "this": {}, "to": {},
	"use": {}, "used": {}, "uses": {}, "when": {}, "which": {}, "with": {},
	"without": {}, "would": {},
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
	//
	// A directive is ranked above every other candidate, ahead of both the
	// similarity and the boost, so a rule a maintainer promoted above the
	// repository's guidelines is never the rule a limit leaves out. It is the one
	// property here that is not a distance, and the returned similarity stays the
	// distance the embedding has. The renderer's own byte cap is a separate bound
	// and can still drop an oversized rule, promoted or not.
	rows, err := d.pool.Query(ctx, `
		SELECT id, scope, repo_name, path_glob, rule_text, rationale, authority, similarity
		FROM (
		    SELECT id, scope, coalesce(repo_name, '') AS repo_name,
		           coalesce(path_glob, '') AS path_glob, rule_text,
		           coalesce(rationale, '') AS rationale, authority,
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
		ORDER BY authority = 'DIRECTIVE' DESC, score DESC, id
		LIMIT $5`,
		vectorLiteral(query.Embedding), query.InstallationID, query.OrgName, query.RepoName, fetch, query.RepoScopeBoost)
	if err != nil {
		return nil, fmt.Errorf("searching memories: %w", err)
	}
	defer rows.Close()

	matches := make([]MemoryMatch, 0, limit)
	for rows.Next() {
		var m MemoryMatch
		if err := rows.Scan(&m.ID, &m.Scope, &m.RepoName, &m.PathGlob, &m.RuleText, &m.Rationale, &m.Authority, &m.Similarity); err != nil {
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
		&m.ID, &m.InstallationID, &m.OrgName, &m.RepoName, &m.Scope, &m.Authority, &m.Status,
		&m.PathGlob, &m.RuleText, &m.Rationale, &m.SourcePR, &m.SourceComment,
		&m.RequestedBy, &m.ReviewedBy, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning a memory row: %w", err)
	}
	return &m, nil
}

// vectorLiteral renders an embedding in the text form pgvector parses. The
// width is the embedder's business: it refuses a provider that does not return
// as many floats as the provider declares, and the column was sized to that
// declaration by EnsureEmbeddingWidth, so the column is never handed the wrong
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
