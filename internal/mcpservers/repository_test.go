package mcpservers_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/snapshot"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func openSnapshot(t *testing.T, files map[string]string) *snapshot.Snapshot {
	t.Helper()
	snap, err := snapshot.Create(context.Background(), bytes.NewReader(testutil.TarGz(t, files)), snapshot.Options{
		MaxBytes: 1 << 20,
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("building the test snapshot: %v", err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	return snap
}

func repositoryTools(t *testing.T, gh *testutil.FakeGitHub, snap *snapshot.Snapshot) *mcpservers.GithubTools {
	t.Helper()
	rumil := ghapp.NewClient(fakeClient(t, gh), gh.Owner, gh.Repo, gh.Number)
	return mcpservers.NewGithubTools(rumil, mcpservers.GithubToolsOptions{
		MaxDiffBytes: 4096,
		BotHandle:    "rumil",
		Logger:       slog.New(slog.DiscardHandler),
		Snapshot:     snap,
		HeadSHA:      gh.HeadSHA,
	})
}

func specNamed(t *testing.T, tools *mcpservers.GithubTools, name string) mcpservers.Spec {
	t.Helper()
	for _, spec := range tools.Specs() {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("the tool set has no %s", name)
	return mcpservers.Spec{}
}

func toolNames(tools *mcpservers.GithubTools) []string {
	specs := tools.Specs()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func TestSnapshotAddsSearchAndKeepsTheReadTools(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	snap := openSnapshot(t, map[string]string{"main.go": "package main\n"})

	names := toolNames(repositoryTools(t, gh, snap))
	for _, want := range []string{"list_repository_tree", "list_directory", "search_code", "get_file_contents"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool set = %v, want %s", names, want)
		}
	}

	without := toolNames(repositoryTools(t, gh, nil))
	if slices.Contains(without, "search_code") {
		t.Errorf("tool set without a snapshot = %v, want no search_code", without)
	}
	if !slices.Contains(without, "list_repository_tree") || !slices.Contains(without, "list_directory") {
		t.Errorf("tool set without a snapshot = %v, want the API-backed listing tools", without)
	}
}

func TestLocalListingAndReading(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	snap := openSnapshot(t, map[string]string{
		"main.go":            "package main\n\nfunc main() {}\n",
		"internal/a.go":      "package internal\n\nfunc A() {}\n",
		"internal/deep/b.go": "package deep\n\nfunc B() {}\n",
		"docs/notes.md":      "# notes\n",
	})
	tools := repositoryTools(t, gh, snap)
	ctx := context.Background()

	tree, err := specNamed(t, tools, "list_repository_tree").Handle(ctx, map[string]any{"prefix": "internal/", "glob": "*.go"})
	if err != nil {
		t.Fatalf("list_repository_tree: %v", err)
	}
	if !strings.Contains(tree, "internal/a.go") || !strings.Contains(tree, "internal/deep/b.go") || strings.Contains(tree, "main.go") {
		t.Errorf("tree = %q, want only the internal Go files", tree)
	}

	dir, err := specNamed(t, tools, "list_directory").Handle(ctx, map[string]any{"path": "internal"})
	if err != nil {
		t.Fatalf("list_directory: %v", err)
	}
	if !strings.Contains(dir, "internal/deep/") || !strings.Contains(dir, "internal/a.go") {
		t.Errorf("directory = %q, want the nested directory and the file", dir)
	}

	body, err := specNamed(t, tools, "get_file_contents").Handle(ctx, map[string]any{
		"file_path": "internal/a.go", "start_line": 1, "end_line": 2,
	})
	if err != nil {
		t.Fatalf("get_file_contents: %v", err)
	}
	if !strings.Contains(body, "package internal") || strings.Contains(body, "func A") {
		t.Errorf("read = %q, want only the first two lines", body)
	}

	if _, err := specNamed(t, tools, "get_file_contents").Handle(ctx, map[string]any{"file_path": "internal/missing.go"}); err == nil ||
		!strings.Contains(err.Error(), "list_repository_tree") {
		t.Errorf("a missing path = %v, want it to name the listing tool", err)
	}
}

func TestLocalSearch(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	snap := openSnapshot(t, map[string]string{
		"reward.go":      "package reward\n\nfunc CreateRewardHandler() {}\n",
		"reward_test.go": "package reward\n",
		"reward.sql":     "SELECT 1;\n",
	})
	tools := repositoryTools(t, gh, snap)

	out, err := specNamed(t, tools, "search_code").Handle(context.Background(), map[string]any{
		"query": "CreateReward", "glob": "*.go",
	})
	if err != nil {
		t.Fatalf("search_code: %v", err)
	}
	if !strings.Contains(out, "reward.go:3:") {
		t.Errorf("search = %q, want the matching line and number", out)
	}
	if strings.Contains(out, "reward_test.go") {
		t.Errorf("search = %q, want the glob to exclude the test file", out)
	}

	none, err := specNamed(t, tools, "search_code").Handle(context.Background(), map[string]any{"query": "not-there"})
	if err != nil {
		t.Fatalf("search_code without a match: %v", err)
	}
	if !strings.Contains(none, "no matches") {
		t.Errorf("search without a match = %q, want an explicit answer", none)
	}
}

func TestListingFallsBackToTheGitHubTree(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Contents["internal/a.go"] = "package internal\n"
	tools := repositoryTools(t, gh, nil)
	ctx := context.Background()

	tree, err := specNamed(t, tools, "list_repository_tree").Handle(ctx, map[string]any{"glob": "*.go"})
	if err != nil {
		t.Fatalf("list_repository_tree: %v", err)
	}
	if !strings.Contains(tree, "main.go") || !strings.Contains(tree, "internal/a.go") {
		t.Errorf("tree = %q, want the fake repository's Go files", tree)
	}

	dir, err := specNamed(t, tools, "list_directory").Handle(ctx, map[string]any{"path": "internal"})
	if err != nil {
		t.Fatalf("list_directory: %v", err)
	}
	if !strings.Contains(dir, "internal/a.go") {
		t.Errorf("directory = %q, want internal/a.go", dir)
	}
}
