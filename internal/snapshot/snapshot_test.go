package snapshot_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/snapshot"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func open(t *testing.T, files map[string]string) *snapshot.Snapshot {
	t.Helper()
	snap, err := snapshot.Create(context.Background(), bytes.NewReader(testutil.TarGz(t, files)), snapshot.Options{
		MaxBytes: 1 << 20,
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	return snap
}

func TestCreateIndexesAndReads(t *testing.T) {
	snap := open(t, map[string]string{
		"main.go":                      "package main\n\nfunc main() {}\n",
		"internal/service.go":          "package internal\n\nfunc Settle() error { return nil }\n",
		"internal/deep/reward.go":      "package deep\n\nfunc Reward() {}\n",
		"internal/deep/reward.sql":     "SELECT 1;\n",
		"docs/notes.md":                "# Notes\n",
		"internal/deep/reward_test.go": "package deep\n",
	})

	if got, want := snap.Count(), 6; got != want {
		t.Fatalf("Count = %d, want %d", got, want)
	}

	all := snap.All()
	if all[0].Path != "docs/notes.md" || all[len(all)-1].Path != "main.go" {
		t.Errorf("All is not sorted: %v", all)
	}

	read, err := snap.Read("internal/service.go", 1, 3)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(read, "func Settle") {
		t.Errorf("Read = %q, want the Settle function", read)
	}

	if _, err := snap.Read("missing.go", 1, 1); err == nil {
		t.Error("Read of a missing file returned no error")
	}
	if _, err := snap.Read("internal/service.go", 99, 100); err == nil {
		t.Error("Read past the end returned no error")
	}
}

func TestReadCapsTheNumberOfLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < snapshot.MaxReadLines+20; i++ {
		b.WriteString("line\n")
	}
	snap := open(t, map[string]string{"long.txt": b.String()})

	text, err := snap.Read("long.txt", 1, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(text, "\n") {
		if line == "line" {
			lines++
		}
	}
	if lines != snapshot.MaxReadLines {
		t.Errorf("Read returned %d lines, want the %d-line cap", lines, snapshot.MaxReadLines)
	}
	if !strings.Contains(text, "continue from line") {
		t.Errorf("a truncated read does not say where to continue: %q", text)
	}
}

func TestBinaryFilesAreRefused(t *testing.T) {
	snap := open(t, map[string]string{
		"image.bin": string([]byte{0x89, 'P', 'N', 'G', 0, 1, 2}),
		"notes.go":  "package notes\n",
	})

	if _, err := snap.Read("image.bin", 1, 1); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("Read of a binary file = %v, want a binary refusal", err)
	}
}

func TestFiltersAndPagesTheTree(t *testing.T) {
	snap := open(t, map[string]string{
		"internal/a.go":      "package internal\n",
		"internal/b.go":      "package internal\n",
		"internal/readme.md": "# internal\n",
		"internal/deep/c.go": "package deep\n",
		"cmd/main.go":        "package main\n",
	})

	files, total, _ := snap.Files("internal/", "*.go", 0, 2)
	if total != 3 {
		t.Errorf("total = %d, want 3 Go files under internal", total)
	}
	if len(files) != 2 || files[0].Path != "internal/a.go" || files[1].Path != "internal/b.go" {
		t.Errorf("first page = %v, want the first two Go files", files)
	}
	next, _, more := snap.Files("internal/", "*.go", 2, 2)
	if more {
		t.Error("the last page reports more files")
	}
	if len(next) != 1 || next[0].Path != "internal/deep/c.go" {
		t.Errorf("second page = %v, want internal/deep/c.go", next)
	}
}

func TestEntriesListsImmediateChildren(t *testing.T) {
	snap := open(t, map[string]string{
		"main.go":            "package main\n",
		"internal/a.go":      "package internal\n",
		"internal/deep/b.go": "package deep\n",
	})

	root, err := snap.Entries("")
	if err != nil {
		t.Fatalf("Entries(root): %v", err)
	}
	if len(root) != 2 || !root[0].Dir || root[0].Path != "internal" || root[1].Path != "main.go" {
		t.Errorf("root entries = %v, want internal/ then main.go", root)
	}

	internal, err := snap.Entries("internal")
	if err != nil {
		t.Fatalf("Entries(internal): %v", err)
	}
	if len(internal) != 2 || !internal[0].Dir || internal[0].Path != "internal/deep" {
		t.Errorf("internal entries = %v, want deep/ and a.go", internal)
	}

	if _, err := snap.Entries("nope"); err == nil {
		t.Error("Entries of a missing directory returned no error")
	}
	if _, err := snap.Entries("main.go"); err == nil {
		t.Error("Entries of a file returned no error")
	}
}

func TestSearchFindsLinesAndNarrows(t *testing.T) {
	snap := open(t, map[string]string{
		"reward.go":        "package reward\n\nfunc CreateRewardHandler() {}\nfunc createReward() {}\n",
		"reward_test.go":   "package reward\n\nfunc TestCreate(t *testing.T) {}\n",
		"unrelated/doc.md": "CreateRewardHandler is described here.\n",
	})

	matches, more, err := snap.Search(context.Background(), "CreateReward", "", "*.go", 10, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if more || len(matches) != 1 || matches[0].Path != "reward.go" || matches[0].Line != 3 {
		t.Errorf("matches = %v (more %t), want reward.go line 3", matches, more)
	}

	insensitive, _, err := snap.Search(context.Background(), "createreward", "", "*.go", 10, true)
	if err != nil {
		t.Fatalf("case-insensitive Search: %v", err)
	}
	if len(insensitive) != 2 {
		t.Errorf("case-insensitive matches = %v, want both spellings", insensitive)
	}

	limited, more, err := snap.Search(context.Background(), "Reward", "", "*.go", 1, false)
	if err != nil {
		t.Fatalf("limited Search: %v", err)
	}
	if len(limited) != 1 || !more {
		t.Errorf("limited matches = %v (more %t), want one match and a truncation", limited, more)
	}

	if _, _, err := snap.Search(context.Background(), "(", "", "", 10, false); err == nil {
		t.Error("an invalid regular expression returned no error")
	}
}

// The archive is untrusted: names that climb out of the tree and links are
// dropped before anything is written.
func TestCreateRefusesEscapingEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, mode byte, body string) {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: mode}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing the header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("writing the body for %s: %v", name, err)
		}
	}
	write("top/ok.txt", tar.TypeReg, "safe\n")
	write("top/../../escaped.txt", tar.TypeReg, "escaped\n")
	write("/absolute.txt", tar.TypeReg, "absolute\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "top/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatalf("writing the symlink header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip: %v", err)
	}

	parent := t.TempDir()
	snap, err := snapshot.Create(context.Background(), &buf, snapshot.Options{MaxBytes: 1 << 20, Dir: parent})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = snap.Close() }()

	files := snap.All()
	if len(files) != 1 || files[0].Path != "ok.txt" {
		t.Fatalf("files = %v, want only ok.txt", files)
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); err == nil {
		t.Error("an archive entry escaped the snapshot directory")
	}
	if _, err := snap.Read("link", 1, 1); err == nil {
		t.Error("the symlink entry was readable")
	}
}

func TestCreateEnforcesTheByteBudget(t *testing.T) {
	_, err := snapshot.Create(context.Background(), bytes.NewReader(testutil.TarGz(t, map[string]string{
		"big.txt": strings.Repeat("x", 4096),
	})), snapshot.Options{MaxBytes: 1024, Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "snapshot.max_bytes") {
		t.Errorf("Create = %v, want the byte budget named", err)
	}
}

func TestCloseRemovesTheTree(t *testing.T) {
	parent := t.TempDir()
	snap, err := snapshot.Create(context.Background(), bytes.NewReader(testutil.TarGz(t, map[string]string{
		"main.go": "package main\n",
	})), snapshot.Options{MaxBytes: 1 << 20, Dir: parent})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := snap.Root()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the snapshot directory does not exist: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the snapshot directory survived Close: %v", err)
	}
}
