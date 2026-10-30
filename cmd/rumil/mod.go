package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/embedder"
	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
)

// printMemories writes the rule queue as a table, which is what the list
// subcommand and the non-interactive form of the browser both print.
func printMemories(out io.Writer, memories []store.Memory) error {
	if len(memories) == 0 {
		_, _ = fmt.Fprintln(out, "no rules match")
		return nil
	}

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tSCOPE\tAUTHORITY\tSTATUS\tAPPLIES TO\tGLOB\tREQUESTED BY\tCREATED\tRULE")
	for _, memory := range memories {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			memory.ShortID(),
			memory.Scope,
			memory.Authority,
			memory.Status,
			moderationTarget(memory),
			orDash(memory.PathGlob),
			memory.RequestedBy,
			memory.CreatedAt.UTC().Format(time.RFC3339),
			oneLine(memory.RuleText),
		)
	}
	return writer.Flush()
}

// defaultModerator is who a decision is recorded against when the operator does
// not name one. It is deliberately not the local user: a rules record belongs
// to the team that writes it, and nothing here reads the host's identity.
const defaultModerator = "cli"

func newModCmd(configPath *string) *cobra.Command {
	var moderator string

	cmd := &cobra.Command{
		Use:   "mod",
		Short: "Review the rules waiting for a maintainer's decision",
		Long: "List, approve and reject the engineering rules Rumil recorded from comments.\n" +
			"Only an approved rule is ever retrieved into a review.\n\n" +
			"On a terminal the command opens the rules in a browser: move through them, open one to read\n" +
			"everything recorded about it, and decide it there. With the input or the error output\n" +
			"redirected there is nothing to move around with, so the queue is printed and the subcommands\n" +
			"below are the surface to use instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, _, closeDB, err := openModeration(*configPath)
			if err != nil {
				return err
			}
			defer closeDB()

			if !isTerminal(cmd.InOrStdin()) || !isTerminal(cmd.ErrOrStderr()) {
				memories, err := service.List(cmd.Context(), store.MemoryFilter{Limit: modTUIListLimit})
				if err != nil {
					return err
				}
				if err := printMemories(cmd.OutOrStdout(), memories); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(),
					"\nto decide one: rumil mod approve <memory-id> --moderator <name>")
				return nil
			}
			return runModTUI(cmd, service, moderator)
		},
	}

	cmd.Flags().StringVar(&moderator, "moderator", defaultModerator, "who a decision made in the browser is recorded against")
	cmd.AddCommand(newModListCmd(configPath))
	cmd.AddCommand(newModApproveCmd(configPath))
	cmd.AddCommand(newModRejectCmd(configPath))
	return cmd
}

func newModListCmd(configPath *string) *cobra.Command {
	var (
		status string
		repo   string
		limit  int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recorded rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, _, closeDB, err := openModeration(*configPath)
			if err != nil {
				return err
			}
			defer closeDB()

			filter := store.MemoryFilter{Status: strings.ToUpper(status), Limit: limit}
			if repo != "" {
				org, name, err := splitRepo(repo)
				if err != nil {
					return err
				}
				filter.OrgName, filter.RepoName = org, name
			}

			memories, err := service.List(cmd.Context(), filter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return printMemories(out, memories)
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "only this status: pending, approved or rejected")
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository, as owner/name")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many rules to list (default 50, max 200)")
	return cmd
}

func newModApproveCmd(configPath *string) *cobra.Command {
	var (
		ruleText  string
		scope     string
		authority string
		moderator string
	)

	cmd := &cobra.Command{
		Use:   "approve <memory-id>",
		Short: "Approve a rule, so later reviews can retrieve it",
		Long: "Approve one rule. The id is the short id a confirmation reply named, or the full UUID.\n" +
			"Passing --rule-text replaces the proposed wording; the replacement is re-embedded, so it is\n" +
			"found by the words that were approved. Passing --scope REPO records an organization-wide rule\n" +
			"for the repository it was proposed in, which is the way to keep a general rule from reaching a\n" +
			"repository it does not fit. The scope is not widened the other way: a repository rule is not\n" +
			"promoted to the whole organization from here.\n" +
			"Passing --authority directive promotes the rule above the repository's own RUMIL.md for the\n" +
			"case it names, which is recorded here and nowhere else. A directive applies to one repository,\n" +
			"so an organization-wide rule is narrowed with --scope REPO in the same command; --authority\n" +
			"context demotes a rule that was promoted earlier.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, _, closeDB, err := openModeration(*configPath)
			if err != nil {
				return err
			}
			defer closeDB()

			memory, err := service.Approve(cmd.Context(), args[0], moderation.Decision{
				RuleText:  ruleText,
				Scope:     scope,
				Authority: authority,
				Moderator: moderator,
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "approved %s (%s/%s, %s, %s, %s)\n",
				memory.ShortID(), memory.OrgName, memory.RepoName, memory.Scope, memory.Authority, memory.Status)
			return nil
		},
	}

	cmd.Flags().StringVar(&ruleText, "rule-text", "", "replace the proposed wording before approving")
	cmd.Flags().StringVar(&scope, "scope", "", "record an organization rule for its repository: REPO narrows it, and a repository rule is not widened to ORG")
	cmd.Flags().StringVar(&authority, "authority", "", "put the rule above the repository's RUMIL.md for one repository: directive promotes it, context leaves it as recorded context")
	cmd.Flags().StringVar(&moderator, "moderator", defaultModerator, "who made the decision, recorded as reviewed_by")
	return cmd
}

func newModRejectCmd(configPath *string) *cobra.Command {
	var moderator string

	cmd := &cobra.Command{
		Use:   "reject <memory-id>",
		Short: "Reject a rule, so no review retrieves it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, _, closeDB, err := openModeration(*configPath)
			if err != nil {
				return err
			}
			defer closeDB()

			memory, err := service.Reject(cmd.Context(), args[0], moderator)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rejected %s (%s/%s, %s)\n",
				memory.ShortID(), memory.OrgName, memory.RepoName, memory.Status)
			return nil
		},
	}

	cmd.Flags().StringVar(&moderator, "moderator", defaultModerator, "who made the decision, recorded as reviewed_by")
	return cmd
}

// moderationTarget names what a rule applies to, which is the organization for an
// ORG-scoped rule and the repository it was recorded in otherwise. The origin
// repository is worth showing for an org rule: that is where the comment was.
func moderationTarget(memory store.Memory) string {
	if memory.Scope == store.ScopeOrg {
		return memory.OrgName + " (proposed in " + memory.RepoName + ")"
	}
	return memory.OrgName + "/" + memory.RepoName
}

// openModeration builds the service a moderation command needs: the configured
// database, and the embedding provider an edit needs. A missing provider is not
// an error here, because listing and rejecting do not need one.
func openModeration(configPath string) (*moderation.Service, *slog.Logger, func(), error) {
	cfg, path, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	logger := newLogger(cfg.App.LogLevel)
	logger.Info("configuration loaded", "path", path)
	warnAboutModeration(cfg, logger, false)

	if cfg.Database.DSN == "" {
		return nil, nil, nil, fmt.Errorf("database.dsn is required to read the recorded rules")
	}
	primary := cfg.GitHub.PrimaryInstallationID()
	if primary <= 0 {
		return nil, nil, nil, fmt.Errorf("github.installation_id or github.installations is required to scope the recorded rules")
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.Database.DSN, cfg.Database.AutoMigrate, cfg.Models.EmbeddingDimensions())
	if err != nil {
		return nil, nil, nil, err
	}

	client, err := moderationEmbedder(cfg)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}

	service := moderation.NewService(db, client, primary, logger)
	return service, logger, db.Close, nil
}

// moderationEmbedder builds the embedding client an edited rule is re-embedded
// with, or returns nil when the operator configured no provider. The interface
// is nil-checked by the service, so a nil client is a value here rather than an
// error: memory is opt-in, and this surface has work to do without it.
func moderationEmbedder(cfg *config.Config) (moderation.Embedder, error) {
	provider, err := cfg.Models.EmbeddingProvider()
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, nil
	}
	client, err := embedder.New(provider)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// oneLine condenses a rule onto one line of the table.
func oneLine(text string) string {
	const cap = 80
	line := strings.Join(strings.Fields(text), " ")
	if len(line) > cap {
		return line[:cap] + "…"
	}
	return line
}
