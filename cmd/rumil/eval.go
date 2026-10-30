package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/eval"
)

// newEvalCmd implements `rumil eval`: the reviewer runs against a fixture set,
// with the fake GitHub standing in for a repository, and the findings are
// scored against what each fixture expects.
func newEvalCmd(configPath *string) *cobra.Command {
	var (
		fixtures string
		name     string
		model    string
		asJSON   bool
		timeout  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score the reviewer against a local fixture set",
		Long: "Run the reviewer against a directory of fixtures and report how many expected findings it found.\n" +
			"GitHub is replaced by an in-process fake, so nothing is posted and no repository is read; the configured\n" +
			"chat provider is real. A fixture is a YAML file holding the diff, the files, the findings a good review\n" +
			"should produce, and the findings known to be wrong.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(fixtures) == "" {
				return errors.New("--fixtures is required")
			}

			cfg, path, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			if model != "" {
				cfg.Models.DefaultChat = model
			}
			// The eval measures the prompt and the model. A working tree would
			// need an archive the fake does not serve for the fixture's files,
			// and operator MCP tools would bring their own tools and side
			// effects into a comparison that is about the reviewer.
			snapshot := false
			cfg.Snapshot.Enabled = &snapshot
			cfg.MCPServers = nil

			set, err := eval.LoadDir(fixtures)
			if err != nil {
				return err
			}
			if name != "" {
				set, err = selectFixtures(set, name)
				if err != nil {
					return err
				}
			}

			logger := newLogger(cfg.App.LogLevel).With(
				"config", path,
				"fixtures", fixtures,
				"model", cfg.Models.DefaultChat,
			)
			report := eval.Runner{Config: cfg, Logger: logger, Timeout: timeout}.Run(cmd.Context(), set)

			if asJSON {
				return eval.WriteJSON(cmd.OutOrStdout(), report)
			}
			return eval.WriteTable(cmd.OutOrStdout(), report)
		},
	}

	cmd.Flags().StringVar(&fixtures, "fixtures", "", "directory of *.yaml fixtures (required)")
	cmd.Flags().StringVar(&name, "fixture", "", "run only the fixture with this name")
	cmd.Flags().StringVar(&model, "model", "", "chat provider id to review with, instead of models.default_chat")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "budget per fixture, for example 10m; 0 uses worker.job_timeout")
	_ = cmd.MarkFlagRequired("fixtures")

	return cmd
}

// selectFixtures keeps the named fixture, or fails naming what the set holds.
func selectFixtures(all []eval.Fixture, name string) ([]eval.Fixture, error) {
	for _, fixture := range all {
		if fixture.Name == name {
			return []eval.Fixture{fixture}, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, fixture := range all {
		names = append(names, fixture.Name)
	}
	return nil, fmt.Errorf("no fixture named %q; the set holds %s", name, strings.Join(names, ", "))
}
