package reviewer

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	fantasy "charm.land/fantasy"

	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// The schema a session advertises is the one a provider validates. The
// arguments belong in the tool's "properties" and the required names beside
// them, not nested inside a schema that names itself: a provider that validates
// the request refuses the nested form (internal/testutil.ValidateToolSchemas is
// that provider), and one that does not still shows the model a shape it cannot
// fill in.
func TestToolsAdvertiseTheSpecSchema(t *testing.T) {
	specs := []mcpservers.Spec{
		{
			Name:        "get_pr_metadata",
			Description: "takes no arguments",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handle:      noopHandle,
		},
		{
			Name:        "get_file_contents",
			Description: "reads one file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Repository-relative file path."},"start_line":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}`),
			Handle:      noopHandle,
		},
	}

	tools, err := kitTools(specs, nil)
	if err != nil {
		t.Fatalf("kitTools returned error: %v", err)
	}
	if len(tools) != len(specs) {
		t.Fatalf("kitTools returned %d tools, want %d", len(tools), len(specs))
	}

	// A tool that takes nothing advertises no properties rather than a schema.
	if got := maps.Keys(tools[0].Info().Parameters); len(slices.Collect(got)) != 0 {
		t.Errorf("get_pr_metadata advertises %v, want no arguments", slices.Collect(maps.Keys(tools[0].Info().Parameters)))
	}

	info := tools[1].Info()
	if info.Name != "get_file_contents" || info.Description != "reads one file" {
		t.Errorf("tool is %q/%q, want the spec's name and description", info.Name, info.Description)
	}
	got := slices.Sorted(maps.Keys(info.Parameters))
	if want := []string{"file_path", "start_line"}; !slices.Equal(got, want) {
		t.Errorf("advertised arguments = %v, want %v, and not the words of a schema", got, want)
	}
	if want := []string{"file_path"}; !slices.Equal(info.Required, want) {
		t.Errorf("advertised required = %v, want %v", info.Required, want)
	}

	// The property schemas travel as they were written, descriptions included.
	property, ok := info.Parameters["file_path"].(map[string]any)
	if !ok {
		t.Fatalf("file_path = %T, want a schema object", info.Parameters["file_path"])
	}
	if property["type"] != "string" || property["description"] != "Repository-relative file path." {
		t.Errorf("file_path = %v, want the schema the spec declares", property)
	}
}

// A spec whose schema is not an object cannot describe arguments, and saying so
// at session setup beats sending it to a provider that refuses the request.
func TestKitToolsRefuseASchemaThatIsNotAnObject(t *testing.T) {
	_, err := kitTools([]mcpservers.Spec{{
		Name:   "broken",
		Schema: json.RawMessage(`{"type":"array","items":{"type":"string"}}`),
		Handle: noopHandle,
	}}, nil)
	if err == nil {
		t.Fatal("kitTools accepted a schema that is not an object")
	}
	for _, want := range []string{"broken", "want an object"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// onToolCall still runs, since the engine's turn budget is reported from it.
func TestKitToolsReportEachCall(t *testing.T) {
	called := make([]string, 0, 1)
	specs := []mcpservers.Spec{{
		Name:   "submit_pr_review",
		Schema: json.RawMessage(`{"type":"object","properties":{"event":{"type":"string"}},"required":["event"]}`),
		Handle: func(context.Context, map[string]any) (string, error) { return "submitted", nil },
	}}

	tools, err := kitTools(specs, func(name string) { called = append(called, name) })
	if err != nil {
		t.Fatalf("kitTools returned error: %v", err)
	}
	got, err := tools[0].Run(context.Background(), toolCall("submit_pr_review", `{"event":"COMMENT"}`))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(got.Content, "submitted") {
		t.Errorf("response = %q, want the handler's text", got.Content)
	}
	if !slices.Equal(called, []string{"submit_pr_review"}) {
		t.Errorf("onToolCall ran for %v, want the tool that was called", called)
	}
}

// The schema that reaches a provider is the one fantasy builds around
// ToolInfo: the arguments as "properties" and the required names beside them.
// This is the shape a validating provider reads, and the double refuses
// anything else, so a session that runs is a session such a provider accepts.
func TestTheWireSchemaIsWhatAValidatingProviderReads(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if rejected := llm.RejectedSchemas(); len(rejected) != 0 {
		t.Fatalf("the provider refused %d requests, first: %s", len(rejected), rejected[0])
	}

	// get_pr_metadata takes no arguments, and says so rather than nesting a
	// schema of its own under "properties".
	parameters := llm.ToolSchemaOf("get_pr_metadata")
	if parameters == nil {
		t.Fatal("the provider was shown no schema for get_pr_metadata")
	}
	if parameters["type"] != "object" {
		t.Errorf("type = %v, want object", parameters["type"])
	}
	if properties, ok := parameters["properties"].(map[string]any); !ok || len(properties) != 0 {
		t.Errorf("properties = %v, want no arguments", parameters["properties"])
	}
	if required, ok := parameters["required"].([]any); !ok || len(required) != 0 {
		t.Errorf("required = %v, want an empty array rather than null", parameters["required"])
	}

	// A tool with arguments carries them at the top level, with the required
	// names pointing at properties that exist.
	parameters = llm.ToolSchemaOf("get_file_contents")
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want an object", parameters["properties"])
	}
	if _, ok := properties["file_path"]; !ok {
		t.Errorf("properties = %v, want the file_path argument", properties)
	}
	required, ok := parameters["required"].([]any)
	if !ok || !slices.Contains(required, any("file_path")) {
		t.Errorf("required = %v, want file_path", parameters["required"])
	}
}

func noopHandle(context.Context, map[string]any) (string, error) { return "", nil }

// toolCall is one call as the session delivers it: the arguments as raw JSON.
func toolCall(name, input string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: "call-1", Name: name, Input: input}
}
