package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/joangrigorov/rumil/internal/ghapp"
)

// Bounds on the history block that goes in front of a review. A pull request
// that has been round many times is the case this exists for, so the block has
// to stay readable: the summaries and the open threads are condensed, and the
// full text stays one tool call away (project_plan.md §7.1 rule 4, T-020-07).
const (
	maxHistoryReviews = 10
	maxHistoryThreads = 10
	historyLineCap    = 200
	historyBlockCap   = 6 << 10
)

// readPriorHistory reads what earlier rounds left behind and renders it as the
// <prior_reviews> block. Both reads are best effort: a pull request whose
// history cannot be read is reviewed as a first round rather than not at all.
// The reviews are also returned, because they are the session's baseline: how
// many reviews Rumil had already submitted decides whether it may close a
// thread, and a review this session submits must not count.
func readPriorHistory(ctx context.Context, client *ghapp.Client, botLogin string, logger *slog.Logger) (string, []ghapp.Review) {
	reviews, err := client.Reviews(ctx)
	if err != nil {
		logger.Warn("reading the review history for the prompt failed, the review runs without it", "error", err)
		return "", nil
	}
	threads, err := client.ReviewThreads(ctx)
	if err != nil {
		logger.Warn("reading the review threads for the prompt failed, the review runs without them", "error", err)
		threads = nil
	}
	return renderHistory(reviews, threads, botLogin), reviews
}

// renderHistory renders the block: one line per earlier review, then the
// threads those rounds left open. It returns "" for a pull request no earlier
// round reviewed, so a first review carries no empty block.
func renderHistory(reviews []ghapp.Review, threads []ghapp.ReviewThread, botLogin string) string {
	if len(reviews) == 0 && len(threads) == 0 {
		return ""
	}

	var (
		b        strings.Builder
		lines    int
		open     int
		resolved int
	)
	b.WriteString("<prior_reviews>\n")
	b.WriteString("This pull request has been reviewed before this session. Earlier review summaries, oldest first:\n")
	for _, review := range reviews {
		if lines == maxHistoryReviews {
			b.WriteString("- and more earlier reviews, which list_pr_conversation returns\n")
			break
		}
		if b.Len() > historyBlockCap {
			break
		}
		author := review.Author
		if ghapp.SameLogin(author, botLogin) {
			author += " (you)"
		}
		fmt.Fprintf(&b, "- %s (%s, commit %s): %s\n", author, review.State, shortCommit(review.CommitID), firstLine(review.Body, historyLineCap))
		lines++
	}

	b.WriteString("Threads those rounds left open, with the comment that opened each one:\n")
	for _, thread := range threads {
		if thread.IsResolved {
			resolved++
			continue
		}
		if open == maxHistoryThreads || b.Len() > historyBlockCap {
			b.WriteString("- and more open threads, which list_pr_conversation returns\n")
			break
		}
		root, ok := thread.Root()
		body := "(no comment)"
		if ok {
			body = firstLine(root.Body, historyLineCap)
		}
		fmt.Fprintf(&b, "- %s on %s:%d: %s\n", thread.ID, thread.Path, thread.Line, body)
		open++
	}
	if resolved > 0 {
		noun := "threads are"
		if resolved == 1 {
			noun = "thread is"
		}
		fmt.Fprintf(&b, "%d resolved %s left out; list_pr_conversation returns them with their replies.\n", resolved, noun)
	}
	b.WriteString("</prior_reviews>")
	return b.String()
}

// firstLine condenses a comment body onto one line of at most cap bytes, which
// is what keeps one long comment from filling the block.
func firstLine(text string, cap int) string {
	line := singleLine(text)
	if line == "" {
		return "(empty)"
	}
	if len(line) > cap {
		line = line[:cap] + "…"
	}
	return line
}

// shortCommit is the abbreviated commit a summary was submitted against.
func shortCommit(sha string) string {
	if sha == "" {
		return "unknown"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
