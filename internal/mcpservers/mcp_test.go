package mcpservers_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/mcpservers"
)

// TestSpecsPinTheToolSurface pins the tool surface the model sees: the names,
// and the JSON Schema fields a valid call is built from.
func TestSpecsPinTheToolSurface(t *testing.T) {
	tools := mcpservers.NewGithubTools(nil, 4096, "@rumil", slog.New(slog.DiscardHandler))
	specs := tools.Specs()

	wantNames := []string{
		"get_pr_metadata",
		"get_pr_diff",
		"get_file_contents",
		"list_pr_conversation",
		"resolve_review_thread",
		"submit_pr_review",
	}
	if len(specs) != len(wantNames) {
		t.Errorf("tool set holds %d specs, want %d", len(specs), len(wantNames))
	}

	byName := make(map[string]mcpservers.Spec, len(specs))
	for _, spec := range specs {
		byName[spec.Name] = spec
	}
	for _, name := range wantNames {
		if _, ok := byName[name]; !ok {
			t.Errorf("tool set does not expose %q", name)
		}
	}

	for _, spec := range specs {
		if spec.Description == "" {
			t.Errorf("%s has no description", spec.Name)
		}
		if spec.Handle == nil {
			t.Errorf("%s has no handler", spec.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(spec.Schema, &schema); err != nil {
			t.Errorf("%s has an undecodable schema: %v", spec.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", spec.Name, schema["type"])
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s schema does not forbid additional properties", spec.Name)
		}
	}

	submit := string(byName["submit_pr_review"].Schema)
	for _, want := range []string{"REQUEST_CHANGES", "APPROVE", "summary_body", "comments", "required"} {
		if !strings.Contains(submit, want) {
			t.Errorf("submit_pr_review schema lost %q: %s", want, submit)
		}
	}
	if !strings.Contains(string(byName["get_file_contents"].Schema), "file_path") {
		t.Errorf("get_file_contents schema lost file_path: %s", byName["get_file_contents"].Schema)
	}
	for _, want := range []string{"page", "since", "file_path"} {
		if !strings.Contains(string(byName["get_pr_diff"].Schema), want) {
			t.Errorf("get_pr_diff schema lost %s: %s", want, byName["get_pr_diff"].Schema)
		}
	}
	if !strings.Contains(byName["get_pr_metadata"].Description, "last_reviewed_commit") {
		t.Errorf("get_pr_metadata does not advertise last_reviewed_commit: %s", byName["get_pr_metadata"].Description)
	}
	for _, want := range []string{"include_resolved", "path", "before", "limit"} {
		if !strings.Contains(string(byName["list_pr_conversation"].Schema), want) {
			t.Errorf("list_pr_conversation schema lost %s: %s", want, byName["list_pr_conversation"].Schema)
		}
	}
	resolve := string(byName["resolve_review_thread"].Schema)
	for _, want := range []string{"thread_id", "reason", `"required":["thread_id","reason"]`} {
		if !strings.Contains(resolve, want) {
			t.Errorf("resolve_review_thread schema lost %s: %s", want, resolve)
		}
	}
}
