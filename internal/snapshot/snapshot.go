// Package snapshot materializes one commit of a repository for the reviewer's
// read tools. The working tree is read-only, lives in a temporary directory,
// and is removed when the session ends.
//
// The tree comes from GitHub's archive endpoint rather than a git clone: a
// review judges one commit, so history is dead weight, and the archive keeps
// the runtime image free of git and the reviewer free of a shell.
package snapshot

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// MaxReadBytes is the largest file a read tool opens, matching the
	// contents API's own limit.
	MaxReadBytes = 8 << 20
	// MaxReadLines caps one read, matching ghapp.MaxFileReadLines.
	MaxReadLines = 500
	// maxSearchFileBytes keeps generated or vendored blobs out of a search.
	maxSearchFileBytes = 4 << 20
	// maxSearchLineBytes bounds one line, so a minified file cannot exhaust
	// memory before its result is discarded.
	maxSearchLineBytes = 1 << 20
	// binaryProbeBytes is how much of a file is checked for NUL bytes.
	binaryProbeBytes = 8 << 10
	// defaultMaxFiles bounds the index a pathological archive can build.
	defaultMaxFiles = 200_000
)

// ErrNotFound reports a path that is not in the working tree.
var ErrNotFound = errors.New("not found in the snapshot")

// Options bounds one materialized tree. MaxBytes is required; the caller
// defaults it from the configuration.
type Options struct {
	MaxBytes int64
	MaxFiles int
	// Dir is the parent directory for the working tree. Empty uses the
	// operating system's temporary directory.
	Dir string
}

// File is one path in the tree.
type File struct {
	Path string
	Size int64
}

// Entry is one immediate child of a directory.
type Entry struct {
	Path string
	Dir  bool
	Size int64
}

// Match is one line a search found.
type Match struct {
	Path string
	Line int
	Text string
}

// Snapshot is one extracted working tree.
type Snapshot struct {
	root  *os.Root
	dir   string
	files []File
	sizes map[string]int64
}

// Create extracts a repository tarball into a fresh temporary directory.
//
// Only regular files are written: symlinks, hard links and device entries are
// skipped, so a repository cannot point a later entry outside the tree. Every
// write goes through an os.Root, which confines the extraction even if a name
// slips past the checks here.
func Create(ctx context.Context, archive io.Reader, opts Options) (*Snapshot, error) {
	if opts.MaxBytes <= 0 {
		return nil, errors.New("snapshot: MaxBytes must be positive")
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	dir, err := os.MkdirTemp(opts.Dir, "rumil-snapshot-")
	if err != nil {
		return nil, fmt.Errorf("creating the snapshot directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("opening the snapshot directory: %w", err)
	}
	snap := &Snapshot{root: root, dir: dir, sizes: map[string]int64{}}
	if err := snap.extract(ctx, archive, opts.MaxBytes, maxFiles); err != nil {
		_ = snap.Close()
		return nil, err
	}
	sort.Slice(snap.files, func(i, j int) bool { return snap.files[i].Path < snap.files[j].Path })
	return snap, nil
}

func (s *Snapshot) extract(ctx context.Context, archive io.Reader, maxBytes int64, maxFiles int) error {
	// The compressed stream cannot outgrow the tree budget by much; this only
	// stops a pathological download from being read forever.
	gz, err := gzip.NewReader(io.LimitReader(archive, maxBytes+(64<<20)))
	if err != nil {
		return fmt.Errorf("reading the repository archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the repository archive: %w", err)
		}

		name, ok := archivePath(header.Name)
		if !ok {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := s.mkdirAll(name); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if len(s.files) >= maxFiles {
				return fmt.Errorf("the repository tree has more than %d files", maxFiles)
			}
			if header.Size > MaxReadBytes {
				// Assets and generated blobs are listed but not written: the
				// read tools refuse them by size, and the tree still shows
				// they exist.
				s.files = append(s.files, File{Path: name, Size: header.Size})
				s.sizes[name] = header.Size
				continue
			}
			if written+header.Size > maxBytes {
				return fmt.Errorf("the repository tree is larger than the configured snapshot.max_bytes of %d bytes", maxBytes)
			}
			if err := s.writeFile(name, tr, header.Size); err != nil {
				return err
			}
			written += header.Size
			s.files = append(s.files, File{Path: name, Size: header.Size})
			s.sizes[name] = header.Size
		}
	}
	if len(s.files) == 0 {
		return errors.New("the repository archive held no readable files")
	}
	return nil
}

// archivePath strips the archive's top directory and refuses a name that
// points outside it. The second return says whether the entry is a file or
// directory inside the tree.
func archivePath(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	if name == "" || name == "." || path.IsAbs(name) {
		return "", false
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	// GitHub wraps the tree in "<owner>-<repo>-<sha>/"; an entry without a
	// separator is that wrapper itself.
	_, rel, ok := strings.Cut(clean, "/")
	if !ok || rel == "" {
		return "", false
	}
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

func (s *Snapshot) mkdirAll(name string) error {
	parts := strings.Split(name, "/")
	built := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if built == "" {
			built = part
		} else {
			built += "/" + part
		}
		if err := s.root.Mkdir(built, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("creating %s in the snapshot: %w", built, err)
		}
	}
	return nil
}

func (s *Snapshot) writeFile(name string, body io.Reader, size int64) error {
	if parent := path.Dir(name); parent != "." {
		if err := s.mkdirAll(parent); err != nil {
			return err
		}
	}
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("writing %s to the snapshot: %w", name, err)
	}
	if _, err := io.CopyN(file, body, size); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing %s to the snapshot: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("writing %s to the snapshot: %w", name, err)
	}
	return nil
}

// Close removes the working tree.
func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.root != nil {
		err = s.root.Close()
	}
	if s.dir != "" {
		if removeErr := os.RemoveAll(s.dir); err == nil {
			err = removeErr
		}
	}
	return err
}

// Count is the number of files in the tree.
func (s *Snapshot) Count() int { return len(s.files) }

// All returns a copy of the tree.
func (s *Snapshot) All() []File { return append([]File(nil), s.files...) }

// Files returns one page of the tree, filtered by a path prefix and a glob
// matched against the file name. offset is zero-based. total is the number of
// matches before paging; truncated reports that the page stopped before them.
func (s *Snapshot) Files(prefix, glob string, offset, limit int) (files []File, total int, truncated bool) {
	return Filter(s.files, prefix, glob, offset, limit)
}

// Filter pages a file list the same way Files does, so the GitHub tree fallback
// and the local snapshot answer identically.
func Filter(all []File, prefix, glob string, offset, limit int) (files []File, total int, truncated bool) {
	files = make([]File, 0, limit)
	prefix = strings.TrimPrefix(prefix, "./")
	for _, file := range all {
		if prefix != "" && !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		if glob != "" && !matchGlob(glob, file.Path) {
			continue
		}
		total++
		if total <= offset {
			continue
		}
		if len(files) < limit {
			files = append(files, file)
		} else {
			truncated = true
		}
	}
	return files, total, truncated
}

func matchGlob(glob, name string) bool {
	if ok, err := path.Match(glob, path.Base(name)); err == nil && ok {
		return true
	}
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// Entries lists the immediate children of dir, which is "" or "." for the
// root. Directories come from the paths below them and are reported without a
// size.
func (s *Snapshot) Entries(dir string) ([]Entry, error) {
	return EntriesOf(s.files, dir)
}

// EntriesOf lists a directory from a file list, so the GitHub tree fallback
// and the local snapshot answer identically.
func EntriesOf(all []File, dir string) ([]Entry, error) {
	dir = strings.Trim(path.Clean("/"+strings.TrimPrefix(dir, "./")), "/")
	prefix := ""
	if dir != "" && dir != "." {
		prefix = dir + "/"
	}

	seen := map[string]bool{}
	var entries []Entry
	for _, file := range all {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(file.Path, prefix)
		if rest == "" {
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested {
			entryPath := prefix + name
			if seen[entryPath] {
				continue
			}
			seen[entryPath] = true
			entries = append(entries, Entry{Path: entryPath, Dir: true})
			continue
		}
		entries = append(entries, Entry{Path: file.Path, Size: file.Size})
	}
	if dir != "" && dir != "." && len(entries) == 0 {
		for _, file := range all {
			if file.Path == dir {
				return nil, fmt.Errorf("%s is a file, not a directory", dir)
			}
		}
		return nil, fmt.Errorf("%s is not a directory in the tree", dir)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}

// Read returns lines start through end, both 1-based and inclusive, with the
// same limits and messages the GitHub contents API path uses.
func (s *Snapshot) Read(name string, start, end int) (string, error) {
	if name == "" {
		return "", errors.New("file_path is required")
	}
	size, ok := s.sizes[name]
	if !ok {
		return "", fmt.Errorf("%w: %s is not a file at this commit", ErrNotFound, name)
	}
	if size > MaxReadBytes {
		return "", fmt.Errorf("%s is %d bytes, larger than the %d byte read limit", name, size, MaxReadBytes)
	}

	file, err := s.root.Open(name)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", name)
	}

	raw, err := io.ReadAll(io.LimitReader(file, MaxReadBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	if len(raw) > MaxReadBytes {
		return "", fmt.Errorf("%s is larger than the %d byte read limit", name, MaxReadBytes)
	}
	if bytes.IndexByte(raw[:min(len(raw), binaryProbeBytes)], 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file and cannot be read", name)
	}

	lines := splitLines(string(raw))
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "", fmt.Errorf("%s has %d lines, start_line %d is past the end", name, len(lines), start)
	}
	if end < 1 || end > len(lines) {
		end = len(lines)
	}

	selected := lines[start-1 : end]
	truncated := false
	if len(selected) > MaxReadLines {
		selected = selected[:MaxReadLines]
		truncated = true
	}
	out := strings.Join(selected, "\n")
	if truncated {
		out += fmt.Sprintf("\n[truncated after %d lines; continue from line %d]", MaxReadLines, start+MaxReadLines)
	}
	return out, nil
}

// Search runs a regular expression over the lines of the files that match the
// prefix and glob. It returns at most limit matches and says whether more
// remained.
func (s *Snapshot) Search(ctx context.Context, query, prefix, glob string, limit int, ignoreCase bool) (matches []Match, truncated bool, err error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("query is required")
	}
	pattern := query
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("query is not a valid regular expression: %w", err)
	}

	prefix = strings.TrimPrefix(prefix, "./")
	for _, file := range s.files {
		if len(matches) >= limit {
			return matches, true, nil
		}
		if err := ctx.Err(); err != nil {
			return matches, false, err
		}
		if prefix != "" && !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		if glob != "" && !matchGlob(glob, file.Path) {
			continue
		}
		if file.Size > maxSearchFileBytes {
			continue
		}

		found, readErr := s.searchFile(re, file.Path, limit-len(matches))
		if readErr != nil {
			// A file that changed under the session is skipped rather than
			// failing the whole search; the tree is private to the session, so
			// this is a disk problem rather than a race.
			continue
		}
		matches = append(matches, found...)
	}
	if len(matches) >= limit {
		return matches, true, nil
	}
	return matches, false, nil
}

func (s *Snapshot) searchFile(re *regexp.Regexp, name string, limit int) ([]Match, error) {
	file, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}

	probe := make([]byte, binaryProbeBytes)
	n, _ := io.ReadFull(file, probe)
	if bytes.IndexByte(probe[:n], 0) >= 0 {
		return nil, errors.New("binary file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var matches []Match
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSearchLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !re.MatchString(text) {
			continue
		}
		matches = append(matches, Match{Path: name, Line: line, Text: clip(text, 300)})
		if len(matches) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return matches, err
	}
	return matches, nil
}

func clip(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

// splitLines matches the contents API path's line split, so both read paths
// number lines the same way.
func splitLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

// FormatSize renders a byte count for a tool result.
func FormatSize(size int64) string {
	switch {
	case size >= 1<<20:
		return strconv.FormatFloat(float64(size)/(1<<20), 'f', 1, 64) + " MB"
	case size >= 1<<10:
		return strconv.FormatFloat(float64(size)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatInt(size, 10) + " B"
	}
}

// Root returns the extracted directory. It exists for tests and is not part of
// the tools' path: reads go through Snapshot, which keeps the confinement.
func (s *Snapshot) Root() string { return s.dir }
