package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
)

// modTUIListLimit is how many rules the browser holds at once. The queue is
// worked from the top, and a list longer than a screenful is one the operator
// scrolls rather than reads.
const modTUIListLimit = 200

// modTUIQuit is the entry that leaves the browser, so quitting is a choice on
// the list rather than a key the operator has to know.
const modTUIQuit = "\u2014 quit \u2014"

// The decisions the rule screen offers.
const (
	modTUIActionApprove = "approve"
	modTUIActionEdit    = "edit"
	modTUIActionReject  = "reject"
	modTUIActionPromote = "promote"
	modTUIActionBack    = "back"
)

// modTUI is the interactive moderation surface: the recorded rules, one rule
// opened, and the decisions a maintainer makes on it. Every decision goes
// through the same service the subcommands use, so the gate, the re-embedding
// of an edited rule and the recorded moderator are the same on both surfaces.
type modTUI struct {
	service   *moderation.Service
	moderator string
	in        io.Reader
	out       io.Writer
}

// runModTUI works the queue until the operator quits. It needs a terminal on
// both sides, which the caller checks: huh reads keys from the input and
// measures itself against the output.
func runModTUI(cmd *cobra.Command, service *moderation.Service, moderator string) error {
	tui := &modTUI{
		service:   service,
		moderator: moderator,
		in:        cmd.InOrStdin(),
		out:       cmd.ErrOrStderr(),
	}
	return tui.run(cmd.Context())
}

func (t *modTUI) run(ctx context.Context) error {
	for {
		memories, err := t.rules(ctx)
		if err != nil {
			return err
		}
		if len(memories) == 0 {
			_, _ = fmt.Fprintln(t.out, "no rules recorded")
			return nil
		}

		choice, err := t.pick(memories)
		if err != nil {
			return aborted(err)
		}
		if choice == modTUIQuit {
			return nil
		}

		index := slices.IndexFunc(memories, func(m store.Memory) bool { return m.ShortID() == choice })
		if index < 0 {
			continue
		}
		if err := t.open(ctx, memories[index]); err != nil {
			return aborted(err)
		}
	}
}

// rules reads the queue, pending first: a decision is what a maintainer came
// for, and an approved rule is only opened to promote it or change it back.
func (t *modTUI) rules(ctx context.Context) ([]store.Memory, error) {
	memories, err := t.service.List(ctx, store.MemoryFilter{Limit: modTUIListLimit})
	if err != nil {
		return nil, err
	}
	rank := map[string]int{store.MemoryPending: 0, store.MemoryApproved: 1, store.MemoryRejected: 2}
	slices.SortStableFunc(memories, func(a, b store.Memory) int {
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] - rank[b.Status]
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return memories, nil
}

// pick is the list screen: one line per rule, with the status, the scope and
// the wording, and the entry that leaves.
func (t *modTUI) pick(memories []store.Memory) (string, error) {
	options := make([]huh.Option[string], 0, len(memories)+1)
	for _, memory := range memories {
		options = append(options, huh.NewOption(ruleLine(memory), memory.ShortID()))
	}
	options = append(options, huh.NewOption(modTUIQuit, modTUIQuit))

	var choice string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("%d recorded rules", len(memories))).
				Description("Enter opens one. Approving a rule is what makes it retrievable.").
				Options(options...).
				Value(&choice).
				Height(min(len(options), tuiListHeight)),
		),
	).WithInput(t.in).WithOutput(t.out)
	if err := form.Run(); err != nil {
		return "", err
	}
	return choice, nil
}

// tuiListHeight is how many rules the list shows at once.
const tuiListHeight = 15

// open is the rule screen: the rule itself, everything recorded about it, and
// the decisions. It returns when the operator goes back to the list.
func (t *modTUI) open(ctx context.Context, memory store.Memory) error {
	var action string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title(fmt.Sprintf("%s  %s", memory.ShortID(), memory.Status)).
				Description(ruleDetail(memory)),
			huh.NewSelect[string]().
				Title("What now").
				Options(
					huh.NewOption("Approve, so later reviews retrieve it", modTUIActionApprove),
					huh.NewOption("Edit the wording, then approve", modTUIActionEdit),
					huh.NewOption("Reject, so no review retrieves it", modTUIActionReject),
					huh.NewOption("Approve as a directive, above the repository's RUMIL.md", modTUIActionPromote),
					huh.NewOption("Back to the list", modTUIActionBack),
				).
				Value(&action),
		),
	).WithInput(t.in).WithOutput(t.out)
	if err := form.Run(); err != nil {
		return err
	}

	switch action {
	case modTUIActionApprove:
		return t.decide(ctx, memory, moderation.Decision{Moderator: t.moderator})
	case modTUIActionEdit:
		return t.edit(ctx, memory)
	case modTUIActionReject:
		_, err := t.service.Reject(ctx, memory.ShortID(), t.moderator)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(t.out, "rejected %s\n", memory.ShortID())
		return nil
	case modTUIActionPromote:
		return t.decide(ctx, memory, moderation.Decision{Authority: store.AuthorityDirective, Moderator: t.moderator})
	default:
		return nil
	}
}

// edit is the wording screen: the rule as it stands, editable, and approved
// with what the operator leaves behind. A rule edited here is re-embedded, so
// it is found by the words that were approved rather than the words that were
// proposed.
func (t *modTUI) edit(ctx context.Context, memory store.Memory) error {
	text := memory.RuleText
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("The rule as it was recorded").
				Description(ruleDetail(memory)),
			huh.NewText().
				Title("The rule to approve").
				Description("Approving re-embeds this text, and this text is what a review reads.").
				Value(&text).
				CharLimit(modTUIRuleCharLimit),
		),
	).WithInput(t.in).WithOutput(t.out)
	if err := form.Run(); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		_, _ = fmt.Fprintln(t.out, "an empty rule is not approved")
		return nil
	}
	return t.decide(ctx, memory, moderation.Decision{RuleText: text, Moderator: t.moderator})
}

// decide approves the rule and reports what the store holds afterwards.
func (t *modTUI) decide(ctx context.Context, memory store.Memory, decision moderation.Decision) error {
	approved, err := t.service.Approve(ctx, memory.ShortID(), decision)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(t.out, "approved %s (%s/%s, %s, %s, %s)\n",
		approved.ShortID(), approved.OrgName, approved.RepoName, approved.Scope, approved.Authority, approved.Status)
	return nil
}

// modTUIRuleCharLimit bounds an edited rule. The rule a maintainer writes is a
// sentence or two; the limit is there so the prompt budget cannot be spent from
// this screen by accident.
const modTUIRuleCharLimit = 2000

// ruleLine is one rule on the list: what it is, where it came from, and enough
// of the wording to recognize it.
func ruleLine(memory store.Memory) string {
	return fmt.Sprintf("%s  %-8s %-4s %-28s %s",
		memory.ShortID(), memory.Status, memory.Scope, moderationTarget(memory), oneLine(memory.RuleText))
}

// ruleDetail is everything recorded about one rule, which is what a maintainer
// needs to decide it without opening the database.
func ruleDetail(memory store.Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", memory.RuleText)
	if memory.Rationale != "" {
		fmt.Fprintf(&b, "Why it was recorded: %s\n", memory.Rationale)
	}
	fmt.Fprintf(&b, "Applies to: %s\n", moderationTarget(memory))
	fmt.Fprintf(&b, "Scope: %s   Authority: %s   Glob: %s\n", memory.Scope, memory.Authority, orDash(memory.PathGlob))
	fmt.Fprintf(&b, "Recorded from: pull request #%d by %s\n", memory.SourcePR, memory.RequestedBy)
	if memory.SourceComment != "" {
		fmt.Fprintf(&b, "Comment: %s\n", memory.SourceComment)
	}
	fmt.Fprintf(&b, "Created: %s\n", memory.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	if memory.ReviewedBy != "" {
		fmt.Fprintf(&b, "Decided by: %s\n", memory.ReviewedBy)
	}
	return b.String()
}

// aborted turns huh's abort into a quiet exit, which is what ctrl-c on a screen
// means: the operator left, and nothing else is wrong.
func aborted(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	return err
}
