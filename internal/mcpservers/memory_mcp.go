package mcpservers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/joangrigorov/rumil/internal/store"
)

// Embedder turns text into the vector a memory search compares against.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// MemorySession is the tenant, and the comment, a memory tool set belongs to.
// The model supplies none of it: the values come from the webhook that started
// the session, so a rule can only be recorded against the repository whose
// comment asked for it.
type MemorySession struct {
	InstallationID int64
	OrgName        string
	RepoName       string
	PRNumber       int
	// SourceComment points a moderator at the comment the rule came from.
	SourceComment string
	RequestedBy   string
	// AutoApprove stores proposals APPROVED instead of PENDING. It is true when
	// moderation.accept_all is on, or when the commenter's
	// author_association is one the operator listed in
	// moderation.auto_approve_associations. With it off, comment text waits for
	// a human decision before it can reach a review prompt.
	AutoApprove bool
	// OrgScoped is true when the command that opened this session asked for an
	// organization-wide rule. It is what makes scope=ORG available to the
	// model: the model never decides who a rule belongs to, and a repository
	// command cannot record a rule that reaches the whole organization by
	// getting the model to say so (project_plan.md §6 Phase 6).
	OrgScoped bool
	// RepoScopeBoost is the weight a REPO-scoped rule gets over an ORG-scoped one
	// when a search ranks candidates.
	RepoScopeBoost float64
}

// MemoryTools holds the dependencies of the memory tool set.
type MemoryTools struct {
	store    *store.DB
	embedder Embedder
	session  MemorySession
	logger   *slog.Logger

	// OnMemoryProposed is called once a proposal reaches the store, so the
	// caller can answer the comment that asked for it.
	OnMemoryProposed func(memory store.Memory, inserted bool)
}

// NewMemoryTools scopes the tools to one session.
func NewMemoryTools(db *store.DB, embedder Embedder, session MemorySession, logger *slog.Logger) *MemoryTools {
	return &MemoryTools{store: db, embedder: embedder, session: session, logger: logger}
}

// SearchSpecs is the tool set a review gets: looking a recorded rule up while
// reading a diff.
func (t *MemoryTools) SearchSpecs() []Spec {
	return []Spec{
		{
			Name:        "search_memory",
			Description: "Search the team's approved engineering rules by meaning, so a convention that was recorded earlier is not reported as a mistake. Returns the closest rules, each with the authority it carries: DIRECTIVE is a rule a maintainer promoted above the repository guidelines, and CONTEXT is recorded context those guidelines outrank. Pass file_path to keep only rules whose glob covers that file.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look up, written as a sentence."},"file_path":{"type":"string","description":"Repository-relative path; only rules whose glob covers it are returned."},"limit":{"type":"integer","description":"How many rules to return, 1 to 15, default 5."}},"required":["query"],"additionalProperties":false}`),
			Handle:      t.searchMemory,
		},
	}
}

// ProposeSpecs is the tool set the synthesis session gets: recording one rule
// distilled from a comment. A session that records rules has no reason to
// search them, and the session that reads them has no reason to write one, so
// the two surfaces stay apart.
//
// The scope the model may ask for is built from the session: a command that did
// not ask for an organization rule is not offered one, so the tool cannot be
// talked into recording a rule that reaches every repository in the
// organization.
func (t *MemoryTools) ProposeSpecs() []Spec {
	return []Spec{
		{
			Name:        "propose_memory",
			Description: t.proposeDescription(),
			Schema:      json.RawMessage(t.proposeSchema()),
			Handle:      t.proposeMemory,
		},
	}
}

// proposeDescription says what the tool does, including the scope this session
// may record for. The default it names is the one the handler applies: a
// command that asked for an organization rule records one unless the model says
// otherwise, because that is what the person asked for, and the schema and the
// description have to tell the model the same story.
func (t *MemoryTools) proposeDescription() string {
	description := "Record one engineering rule distilled from a review conversation, with the reason it exists. " +
		"The rule is stored and moderated unless the operator turned moderation off."
	if t.session.OrgScoped {
		return description + " This command asked for an organization-wide rule: ORG, the default, records a rule that " +
			"holds for every repository in " + t.session.OrgName + "; pass REPO for one that only holds here."
	}
	return description + " The rule applies to the repository it was recorded in."
}

// proposeSchema is the tool schema for this session. The scope property is only
// offered when the command asked for an organization rule, and then both values
// are allowed: the distiller may find that what the person described is really
// about this repository.
func (t *MemoryTools) proposeSchema() string {
	scope := `{"type":"string","enum":["REPO"],"description":"REPO, the default, keeps the rule in this repository."}`
	required := `"required":["rule_text","rationale"],`
	if t.session.OrgScoped {
		scope = `{"type":"string","enum":["REPO","ORG"],"description":"ORG, the default for this command, records the rule for every repository in the organization. REPO keeps it in this one."}`
	}
	return `{"type":"object","properties":{"rule_text":{"type":"string","description":"The rule as one self-contained sentence, stating what to do rather than what happened in the conversation."},"rationale":{"type":"string","description":"Why the rule exists."},"path_glob":{"type":"string","description":"Optional glob limiting the rule to the files it applies to, for example 'internal/payments/**'."},"scope":` +
		scope + `},` + required + `"additionalProperties":false}`
}

func (t *MemoryTools) searchMemory(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	embedding, err := t.embedder.Embed(ctx, query)
	if err != nil {
		return "", fmt.Errorf("search_memory failed: %w", err)
	}

	filePath, _ := args["file_path"].(string)
	matches, err := t.store.SearchMemories(ctx, store.MemoryQuery{
		InstallationID: t.session.InstallationID,
		OrgName:        t.session.OrgName,
		RepoName:       t.session.RepoName,
		Embedding:      embedding,
		FilePath:       filePath,
		RepoScopeBoost: t.session.RepoScopeBoost,
		Limit:          intArg(args, "limit"),
	})
	if err != nil {
		return "", fmt.Errorf("search_memory failed: %w", err)
	}

	type rule struct {
		ID         string  `json:"id"`
		Scope      string  `json:"scope"`
		Repo       string  `json:"repo,omitempty"`
		Authority  string  `json:"authority"`
		PathGlob   string  `json:"path_glob,omitempty"`
		Rule       string  `json:"rule"`
		Rationale  string  `json:"rationale,omitempty"`
		Similarity float64 `json:"similarity"`
	}
	rules := make([]rule, 0, len(matches))
	for _, m := range matches {
		repo := ""
		if m.Scope == store.ScopeRepo {
			repo = t.session.OrgName + "/" + m.RepoName
		}
		rules = append(rules, rule{
			ID:         m.ID,
			Scope:      m.Scope,
			Repo:       repo,
			Authority:  m.Authority,
			PathGlob:   m.PathGlob,
			Rule:       m.RuleText,
			Rationale:  m.Rationale,
			Similarity: m.Similarity,
		})
	}
	return marshalJSON(map[string]any{"rules": rules, "count": len(rules)})
}

func (t *MemoryTools) proposeMemory(ctx context.Context, args map[string]any) (string, error) {
	ruleText, _ := args["rule_text"].(string)
	rationale, _ := args["rationale"].(string)
	if ruleText == "" {
		return "", fmt.Errorf("rule_text is required")
	}
	if rationale == "" {
		return "", fmt.Errorf("rationale is required")
	}

	var notes []string
	scope, _ := args["scope"].(string)
	switch scope = strings.ToUpper(strings.TrimSpace(scope)); scope {
	case "":
		if t.session.OrgScoped {
			// The command asked for an organization rule, so that is what an
			// unstated scope means; the schema says so too.
			scope = store.ScopeOrg
		} else {
			scope = store.ScopeRepo
		}
	case store.ScopeRepo:
	case store.ScopeOrg:
		if !t.session.OrgScoped {
			// The model was not offered ORG in this session, so it is going
			// beyond its brief. Recording the rule for the repository instead
			// would look like a success while storing something the person did
			// not ask for, so the call fails and the session has to answer for
			// it.
			return "", fmt.Errorf("scope ORG needs a command that asked for an organization rule, which this one did not")
		}
	default:
		return "", fmt.Errorf("scope must be REPO or ORG, got %q", scope)
	}

	embedding, err := t.embedder.Embed(ctx, ruleText)
	if err != nil {
		return "", fmt.Errorf("propose_memory failed: %w", err)
	}

	pathGlob, _ := args["path_glob"].(string)
	memory, inserted, err := t.store.ProposeMemory(ctx, store.Proposal{
		InstallationID: t.session.InstallationID,
		OrgName:        t.session.OrgName,
		RepoName:       t.session.RepoName,
		Scope:          scope,
		RuleText:       ruleText,
		Rationale:      rationale,
		PathGlob:       pathGlob,
		SourcePR:       t.session.PRNumber,
		SourceComment:  t.session.SourceComment,
		RequestedBy:    t.session.RequestedBy,
		Approved:       t.session.AutoApprove,
		Embedding:      embedding,
	})
	if err != nil {
		return "", fmt.Errorf("propose_memory failed: %w", err)
	}
	if !inserted {
		notes = append(notes, "an equivalent rule is already recorded as "+memory.ShortID())
	}

	if t.OnMemoryProposed != nil {
		t.OnMemoryProposed(memory, inserted)
	}
	t.logger.Info("memory proposal recorded",
		"id", memory.ID,
		"status", memory.Status,
		"inserted", inserted,
		"repo", memory.OrgName+"/"+memory.RepoName,
		"requested_by", memory.RequestedBy,
	)

	result := map[string]any{
		"id":       memory.ID,
		"short_id": memory.ShortID(),
		"status":   memory.Status,
		"scope":    memory.Scope,
		"repo":     memory.OrgName + "/" + memory.RepoName,
		"created":  inserted,
	}
	if len(notes) > 0 {
		result["note"] = strings.Join(notes, "; ")
	}
	return marshalJSON(result)
}
