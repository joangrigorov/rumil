package testutil

import "fmt"

// ValidateToolSchemas checks the tools of a chat request the way a provider that
// validates them does, so a request that such a provider would refuse cannot be
// sent to the double either.
//
// It is the guard for the nesting described in internal/reviewer/tools.go: a
// JSON Schema handed to kit as the tool's parameters arrives here as
// "properties": {"type": "object", "properties": {...}, ...}, because the
// provider library reads that field as the schema's properties and builds the
// request around it. Both checks below catch that shape, and the first one
// reports it in the words the provider used.
func ValidateToolSchemas(body map[string]any) error {
	tools, _ := body["tools"].([]any)
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)

		parameters, ok := fn["parameters"].(map[string]any)
		if !ok {
			return fmt.Errorf("invalid schema for function %q: parameters is not an object", name)
		}
		if kind, ok := parameters["type"].(string); ok && kind != "object" {
			return fmt.Errorf("invalid schema for function %q: type is %q, want \"object\"", name, kind)
		}

		properties := map[string]any{}
		if raw, present := parameters["properties"]; present {
			properties, ok = raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid schema for function %q: properties is not an object", name)
			}
		}
		for _, property := range properties {
			switch property.(type) {
			case map[string]any, bool:
			default:
				return fmt.Errorf("invalid schema for function %q: %v is not of types \"boolean\", \"object\"", name, property)
			}
		}

		if raw, present := parameters["required"]; present {
			required, ok := raw.([]any)
			if !ok {
				return fmt.Errorf("invalid schema for function %q: required is not an array", name)
			}
			for _, entry := range required {
				field, _ := entry.(string)
				if _, declared := properties[field]; !declared {
					return fmt.Errorf("invalid schema for function %q: required names %q, which properties does not define", name, field)
				}
			}
		}
	}
	return nil
}

// ToolSchemas returns the schema of every tool of a chat request, keyed by the
// function name, which is what a test reads when it cares about the shape the
// provider is shown rather than that a call happened.
func ToolSchemas(body map[string]any) map[string]map[string]any {
	schemas := map[string]map[string]any{}
	tools, _ := body["tools"].([]any)
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if parameters, ok := fn["parameters"].(map[string]any); ok {
			schemas[name] = parameters
		}
	}
	return schemas
}
