package ghapp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// LineKind classifies one line of a unified diff.
type LineKind uint8

// Diff line kinds.
const (
	LineContext LineKind = iota
	LineAdded
	LineRemoved
)

func (k LineKind) marker() string {
	switch k {
	case LineAdded:
		return "+"
	case LineRemoved:
		return "-"
	default:
		return " "
	}
}

// DiffLine carries both the old and the new line number of a diff line so the
// reviewer never has to compute line numbers itself. A zero line number means
// the line does not exist on that side.
type DiffLine struct {
	Kind    LineKind
	OldLine int
	NewLine int
	Text    string
}

// FileDiff is the section of a unified diff describing a single file.
type FileDiff struct {
	Path      string
	OldPath   string
	Additions int
	Deletions int
	Binary    bool
	Lines     []DiffLine
}

// Diff is a parsed unified diff.
type Diff struct {
	Files []FileDiff
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ParseUnifiedDiff parses the unified diff returned by the GitHub compare and
// pull request endpoints into per-file line maps.
func ParseUnifiedDiff(raw string) (*Diff, error) {
	diff := &Diff{}
	var file *FileDiff
	var oldLine, newLine int
	inHunk := false

	for _, line := range splitLines(raw) {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, newPath := gitHeaderPaths(line)
			diff.Files = append(diff.Files, FileDiff{Path: newPath, OldPath: oldPath})
			file = &diff.Files[len(diff.Files)-1]
			inHunk = false

		case strings.HasPrefix(line, "--- "):
			if file == nil {
				diff.Files = append(diff.Files, FileDiff{})
				file = &diff.Files[len(diff.Files)-1]
			}
			file.OldPath = stripDiffPrefix(strings.TrimPrefix(line, "--- "))
			inHunk = false

		case strings.HasPrefix(line, "+++ "):
			if file == nil {
				diff.Files = append(diff.Files, FileDiff{})
				file = &diff.Files[len(diff.Files)-1]
			}
			if path := stripDiffPrefix(strings.TrimPrefix(line, "+++ ")); path != "" {
				file.Path = path
			}
			inHunk = false

		case strings.HasPrefix(line, "@@"):
			m := hunkHeaderRe.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("malformed hunk header %q", line)
			}
			if file == nil {
				diff.Files = append(diff.Files, FileDiff{})
				file = &diff.Files[len(diff.Files)-1]
			}
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = true

		case !inHunk:
			// File metadata between hunks: index lines, mode changes, rename
			// headers, and binary markers all land here.
			if file != nil && strings.HasPrefix(line, "Binary files ") {
				file.Binary = true
			}

		default:
			if file == nil {
				continue
			}
			kind, text, ok := classifyBodyLine(line)
			if !ok {
				inHunk = false
				continue
			}
			switch kind {
			case LineAdded:
				file.Lines = append(file.Lines, DiffLine{Kind: LineAdded, NewLine: newLine, Text: text})
				file.Additions++
				newLine++
			case LineRemoved:
				file.Lines = append(file.Lines, DiffLine{Kind: LineRemoved, OldLine: oldLine, Text: text})
				file.Deletions++
				oldLine++
			default:
				file.Lines = append(file.Lines, DiffLine{Kind: LineContext, OldLine: oldLine, NewLine: newLine, Text: text})
				oldLine++
				newLine++
			}
		}
	}

	return diff, nil
}

func classifyBodyLine(line string) (LineKind, string, bool) {
	if line == "" {
		return LineContext, "", false
	}
	if strings.HasPrefix(line, `\ No newline at end of file`) {
		return LineContext, "", false
	}
	switch line[0] {
	case '+':
		return LineAdded, line[1:], true
	case '-':
		return LineRemoved, line[1:], true
	case ' ':
		return LineContext, line[1:], true
	default:
		return LineContext, "", false
	}
}

func gitHeaderPaths(line string) (oldPath, newPath string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	idx := strings.LastIndex(rest, " b/")
	if idx < 0 {
		return "", ""
	}
	return strings.TrimPrefix(rest[:idx], "a/"), strings.TrimPrefix(rest[idx+1:], "b/")
}

func stripDiffPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	s = strings.TrimPrefix(s, "a/")
	s = strings.TrimPrefix(s, "b/")
	return s
}

func splitLines(raw string) []string {
	return strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
}

// File returns the parsed section for path, or nil when the path is not part
// of the diff.
func (d *Diff) File(path string) *FileDiff {
	for i := range d.Files {
		if d.Files[i].Path == path {
			return &d.Files[i]
		}
	}
	return nil
}

// Paths returns the changed file paths in diff order.
func (d *Diff) Paths() []string {
	paths := make([]string, 0, len(d.Files))
	for i := range d.Files {
		paths = append(paths, d.Files[i].Path)
	}
	return paths
}

// HasNewLine reports whether line exists on the new side of path's diff. Only
// such lines may carry an inline review comment.
func (d *Diff) HasNewLine(path string, line int) bool {
	file := d.File(path)
	if file == nil {
		return false
	}
	for _, l := range file.Lines {
		if l.NewLine == line && l.Kind != LineRemoved {
			return true
		}
	}
	return false
}

// Annotate renders the diff with explicit line numbers in the
// "[OLD:12 NEW:15] + code" form, optionally limited to a single file.
func (d *Diff) Annotate(path string) string {
	files := d.Files
	if path != "" {
		file := d.File(path)
		if file == nil {
			return ""
		}
		files = []FileDiff{*file}
	}

	var b strings.Builder
	for i := range files {
		file := &files[i]
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "file: %s (+%d/-%d)\n", file.Path, file.Additions, file.Deletions)
		if file.Binary {
			b.WriteString("binary file, contents not shown\n")
			continue
		}
		for _, l := range file.Lines {
			b.WriteString("[")
			b.WriteString(lineNumberLabel("OLD", l.OldLine))
			b.WriteString(" ")
			b.WriteString(lineNumberLabel("NEW", l.NewLine))
			b.WriteString("] ")
			b.WriteString(l.Kind.marker())
			b.WriteString(" ")
			b.WriteString(l.Text)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func lineNumberLabel(side string, number int) string {
	if number == 0 {
		return side + ":-"
	}
	return fmt.Sprintf("%s:%d", side, number)
}

// AnnotatedPages renders the annotated diff split into pages of at most
// maxBytes bytes, breaking on line boundaries.
func (d *Diff) AnnotatedPages(maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = int(^uint(0) >> 1)
	}

	lines := splitLines(d.Annotate(""))
	var pages []string
	var current []string
	size := 0

	for _, line := range lines {
		lineSize := len(line) + 1
		if size+lineSize > maxBytes && len(current) > 0 {
			pages = append(pages, strings.Join(current, "\n"))
			current = nil
			size = 0
		}
		current = append(current, line)
		size += lineSize
	}
	if len(current) > 0 {
		pages = append(pages, strings.Join(current, "\n"))
	}
	if len(pages) == 0 {
		pages = []string{""}
	}
	return pages
}
