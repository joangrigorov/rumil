package repoconfig_test

import (
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/repoconfig"
)

func testModels() config.Models {
	return config.Models{Providers: []config.Provider{
		{ID: "chat-a", Provider: "openai-compatible", ModelName: "model-a"},
		{ID: "chat-b", Provider: "openai-compatible", ModelName: "model-b"},
		{ID: "embed", Provider: "openai-compatible", ModelName: "model-e", Dimensions: 1536},
	}}
}

func parse(t *testing.T, doc string) *repoconfig.Settings {
	t.Helper()
	settings, err := repoconfig.Parse([]byte(doc), testModels())
	if err != nil {
		t.Fatalf("Parse returned error: %v\n%s", err, doc)
	}
	return settings
}

func TestParseReadsTheWholeDocument(t *testing.T) {
	settings := parse(t, `
review:
  resolve_threads: false
  request_changes: false
  ignore_paths: ["docs/**", "**/*.md"]
  default_focus: ["migrations", "deployment"]
  max_inline_comments: 12
models:
  default_chat: "chat-b"
`)

	if settings.ResolveThreads == nil || *settings.ResolveThreads {
		t.Errorf("resolve_threads = %v, want false", settings.ResolveThreads)
	}
	if settings.RequestChanges == nil || *settings.RequestChanges {
		t.Errorf("request_changes = %v, want false", settings.RequestChanges)
	}
	if settings.MaxInlineComments == nil || *settings.MaxInlineComments != 12 {
		t.Errorf("max_inline_comments = %v, want 12", settings.MaxInlineComments)
	}
	if got := strings.Join(settings.IgnorePaths, ","); got != "docs/**,**/*.md" {
		t.Errorf("ignore_paths = %q", got)
	}
	if got := strings.Join(settings.DefaultFocus, ","); got != "migrations,deployment" {
		t.Errorf("default_focus = %q", got)
	}
	if settings.DefaultChat != "chat-b" {
		t.Errorf("default_chat = %q, want chat-b", settings.DefaultChat)
	}
}

func TestEmptyDocumentKeepsTheOperatorsSettings(t *testing.T) {
	operator := config.Review{}
	effective := (*repoconfig.Settings)(nil).Apply(operator)
	if !effective.ResolveThreads {
		t.Error("resolve_threads = false, want the operator default of true")
	}
	if !effective.RequestChanges {
		t.Error("request_changes = false, want the default of true")
	}
	if effective.MaxInlineComments != 0 {
		t.Errorf("max_inline_comments = %d, want no cap", effective.MaxInlineComments)
	}
}

// The operator's settings are the baseline and the repository narrows them; a
// repository file that says nothing leaves the operator's value alone.
func TestApplyLetsTheRepositoryNarrowTheOperator(t *testing.T) {
	off := false
	operator := config.Review{ResolveThreads: &off}

	settings := parse(t, `
review:
  ignore_paths: ["docs/**"]
  request_changes: false
`)
	effective := settings.Apply(operator)
	if effective.ResolveThreads {
		t.Error("resolve_threads = true, want the operator's explicit false to stand")
	}
	if effective.RequestChanges {
		t.Error("request_changes = true, want the repository's false")
	}
	if !effective.Ignores("docs/readme.md") {
		t.Error("the repository's ignore path did not reach the effective settings")
	}
}

// A repository may turn a setting on as well as off: the file is the more
// specific layer.
func TestApplyLetsTheRepositoryOverrideTheOperator(t *testing.T) {
	on := true
	operator := config.Review{ResolveThreads: &on}
	settings := parse(t, "review:\n  resolve_threads: false\n")
	if settings.Apply(operator).ResolveThreads {
		t.Error("resolve_threads = true, want the repository's false")
	}
}

func TestParseRefusesUnknownKeys(t *testing.T) {
	_, err := repoconfig.Parse([]byte("review:\n  auto_approve: true\n"), testModels())
	if err == nil || !strings.Contains(err.Error(), "auto_approve") {
		t.Fatalf("error = %v, want it to name the unknown key", err)
	}
}

func TestParseRefusesCredentialsAndProviderDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"an api key", "models:\n  api_key: \"sk-test\"\n"},
		{"a nested token", "review:\n  token: \"secret\"\n"},
		{"a provider definition", "models:\n  providers:\n    - id: \"mine\"\n      api_key: \"sk-test\"\n"},
		{"a database", "database:\n  dsn: \"postgres://x\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repoconfig.Parse([]byte(tc.doc), testModels())
			if err == nil {
				t.Fatalf("Parse accepted %q", tc.doc)
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("error = %v, want a refusal that says the key is not allowed", err)
			}
		})
	}
}

func TestParseRefusesAProviderTheOperatorDidNotConfigure(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"unknown", "chat-z"},
		{"embedding", "embed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repoconfig.Parse([]byte("models:\n  default_chat: \""+tc.id+"\"\n"), testModels())
			if err == nil {
				t.Fatalf("Parse accepted provider %q", tc.id)
			}
			if !strings.Contains(err.Error(), tc.id) {
				t.Errorf("error = %v, want it to name %q", err, tc.id)
			}
		})
	}
}

func TestParseBoundsTheLists(t *testing.T) {
	manyPaths := make([]string, 51)
	for i := range manyPaths {
		manyPaths[i] = "\"path\""
	}
	manyFocus := make([]string, 6)
	for i := range manyFocus {
		manyFocus[i] = "\"focus\""
	}
	long := strings.Repeat("a", 201)

	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"too many ignore paths", "review:\n  ignore_paths: [" + strings.Join(manyPaths, ",") + "]\n", "at most 50"},
		{"too many focus entries", "review:\n  default_focus: [" + strings.Join(manyFocus, ",") + "]\n", "at most 5"},
		{"a long path", "review:\n  ignore_paths: [\"" + long + "\"]\n", "at most 200"},
		{"an absolute path", "review:\n  ignore_paths: [\"/etc/passwd\"]\n", "absolute"},
		{"a path out of the tree", "review:\n  ignore_paths: [\"../secret\"]\n", "climbs out"},
		{"an empty path", "review:\n  ignore_paths: [\"  \"]\n", "empty"},
		{"a zero inline cap", "review:\n  max_inline_comments: 0\n", "between 1 and 200"},
		{"an oversized inline cap", "review:\n  max_inline_comments: 201\n", "between 1 and 200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repoconfig.Parse([]byte(tc.doc), testModels())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A file that exceeds the byte limit is refused before it is parsed.
func TestParseRefusesAnOversizedFile(t *testing.T) {
	doc := "review:\n  default_focus: [\"" + strings.Repeat("a", repoconfig.MaxBytes) + "\"]\n"
	if _, err := repoconfig.Parse([]byte(doc), testModels()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want the size limit", err)
	}
}

func TestIgnoresMatchesDirectoriesAndAnyDepth(t *testing.T) {
	effective := repoconfig.Effective{IgnorePaths: []string{"docs/**", "**/*.md", "vendor/generated.go"}}
	for path, want := range map[string]bool{
		"docs/readme.rst":           true,
		"docs/nested/deep/file.txt": true,
		"README.md":                 true,
		"internal/pkg/notes.md":     true,
		"vendor/generated.go":       true,
		"internal/pkg/notes.go":     false,
		"docsomething/file.go":      false,
	} {
		if got := effective.Ignores(path); got != want {
			t.Errorf("Ignores(%q) = %v, want %v", path, got, want)
		}
	}
}
