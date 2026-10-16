package testutil

import (
	"encoding/json"
	"strings"
	"testing"
)

// requestWithTools builds what a chat request body looks like with the given
// tool schemas, since that is what the double validates.
func requestWithTools(t *testing.T, tools string) map[string]any {
	t.Helper()

	body := map[string]any{}
	if err := json.Unmarshal([]byte(`{"model":"test","tools":[`+tools+`]}`), &body); err != nil {
		t.Fatalf("building the request: %v", err)
	}
	return body
}

// A provider that validates tool schemas refuses a schema whose properties are
// not schemas. That is what a whole JSON Schema placed where the properties
// belong looks like on the wire, and the double has to say so, or a session
// that no validating provider would run passes here.
func TestValidateToolSchemasRefusesASchemaNestedAsItsOwnProperties(t *testing.T) {
	// The shape internal/reviewer advertises when it hands kit the whole
	// schema: "properties" holds the words of a schema rather than arguments.
	body := requestWithTools(t, `{"type":"function","function":{"name":"get_pr_metadata","parameters":{"type":"object","properties":{"type":"object","properties":{},"additionalProperties":false},"required":["file_path"]}}}`)

	err := ValidateToolSchemas(body)
	if err == nil {
		t.Fatal("ValidateToolSchemas accepted a schema nested inside its own properties")
	}
	for _, want := range []string{"get_pr_metadata", "not of types \"boolean\", \"object\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestValidateToolSchemasAcceptsTheShapesTheSpecsUse(t *testing.T) {
	valid := map[string]string{
		"no arguments":  `{"type":"function","function":{"name":"get_pr_metadata","parameters":{"type":"object","properties":{}}}}`,
		"no properties": `{"type":"function","function":{"name":"get_pr_metadata","parameters":{"type":"object"}}}`,
		"one argument":  `{"type":"function","function":{"name":"get_file_contents","parameters":{"type":"object","properties":{"file_path":{"type":"string","description":"Repository-relative file path."}},"required":["file_path"]}}}`,
		"nested array":  `{"type":"function","function":{"name":"submit_pr_review","parameters":{"type":"object","properties":{"comments":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"}}}},"event":{"type":"string","enum":["COMMENT"]}},"required":["event"]}}}`,
	}
	for name, tools := range valid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateToolSchemas(requestWithTools(t, tools)); err != nil {
				t.Errorf("ValidateToolSchemas refused a valid schema: %v", err)
			}
		})
	}
}

func TestValidateToolSchemasRefusesWhatAProviderWould(t *testing.T) {
	invalid := map[string]struct{ tools, want string }{
		"required names an unknown property": {
			tools: `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["b"]}}}`,
			want:  `required names "b"`,
		},
		"properties is not an object": {
			tools: `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":[]}}}`,
			want:  "properties is not an object",
		},
		"type is not object": {
			tools: `{"type":"function","function":{"name":"f","parameters":{"type":"array"}}}`,
			want:  `type is "array"`,
		},
		"parameters is not an object": {
			tools: `{"type":"function","function":{"name":"f","parameters":"object"}}`,
			want:  "parameters is not an object",
		},
	}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			err := ValidateToolSchemas(requestWithTools(t, tc.tools))
			if err == nil {
				t.Fatal("ValidateToolSchemas accepted a schema a provider refuses")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// ToolSchemas is what a test reads to assert the shape the provider was shown.
func TestToolSchemasReadsTheWireSchema(t *testing.T) {
	body := requestWithTools(t, `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}`)

	schemas := ToolSchemas(body)
	parameters, ok := schemas["f"]
	if !ok {
		t.Fatalf("ToolSchemas returned %v, want the f tool", schemas)
	}
	if parameters["type"] != "object" {
		t.Errorf("type = %v, want object", parameters["type"])
	}
	properties, _ := parameters["properties"].(map[string]any)
	if _, ok := properties["a"]; !ok {
		t.Errorf("properties = %v, want the argument a", properties)
	}
}
