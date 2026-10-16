package reviewer

import (
	"context"
	"encoding/json"
	"fmt"

	fantasy "charm.land/fantasy"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/mcpservers"
)

// kitTools turns the tool specs into kit raw tools for one session.
//
// The specs are registered directly rather than through kit's MCP integration:
// a call stays an in-process Go call, and the session never reads kit's
// MCPToolManager, whose GetTools returns the tool slice without holding the
// mutex the parallel server loader writes under (internal/tools/mcp.go), which
// the race detector reports in every session.
//
// onToolCall, when set, runs before each call so the engine can report the
// session's turn budget.
func kitTools(specs []mcpservers.Spec, onToolCall func(name string)) ([]kit.Tool, error) {
	tools := make([]kit.Tool, 0, len(specs))
	for _, spec := range specs {
		input, err := decodeSpecInput(spec)
		if err != nil {
			return nil, err
		}

		// kit is handed the argument properties alone. fantasy reads
		// ToolInfo.Parameters as that map and nests it under "properties" when
		// it builds the request, so handing it the whole JSON Schema is what
		// advertisedTool exists to undo.
		tool := kit.NewRawTool(spec.Name, spec.Description, input.properties, callHandler(spec, onToolCall))
		tools = append(tools, advertisedTool{
			AgentTool: tool,
			info: fantasy.ToolInfo{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  input.properties,
				Required:    input.required,
			},
		})
	}
	return tools, nil
}

// advertisedTool is a kit raw tool whose advertised schema is the spec's.
//
// kit.NewRawTool is documented to take the input's JSON Schema and assigns that
// whole schema to fantasy's ToolInfo.Parameters, which fantasy reads as the
// schema's *properties*. The arguments therefore went out one level too deep:
// "properties" held the words "type", "properties" and "additionalProperties"
// instead of the arguments the model had to fill in. A provider that validates
// the tool schema rejects the request outright — DeepSeek answers "Invalid
// schema for function 'get_pr_metadata': "object" is not of types "boolean",
// "object"" — and one that does not still shows the model a shape it cannot
// satisfy.
//
// Run stays kit's, so the halt record, the argument decoding and the tool call
// ID in the context behave as before.
type advertisedTool struct {
	fantasy.AgentTool
	info fantasy.ToolInfo
}

// Info returns the schema the provider is shown.
func (t advertisedTool) Info() fantasy.ToolInfo { return t.info }

// SetParallel forwards to the wrapped tool when it supports the optional
// method, so a tool that could run in parallel before this wrapper still can.
func (t advertisedTool) SetParallel(parallel bool) {
	if setter, ok := t.AgentTool.(interface{ SetParallel(bool) }); ok {
		setter.SetParallel(parallel)
	}
}

// specInput is the part of a spec's schema the provider is shown: the argument
// properties and the names of the required ones. fantasy builds the rest of the
// schema around them.
type specInput struct {
	properties map[string]any
	required   []string
}

// decodeSpecInput reads a spec's schema, which project_plan.md §5 describes as
// an object whose "properties" are the tool's arguments.
func decodeSpecInput(spec mcpservers.Spec) (specInput, error) {
	var schema struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(spec.Schema, &schema); err != nil {
		return specInput{}, fmt.Errorf("decoding the schema of %s: %w", spec.Name, err)
	}
	if schema.Type != "object" {
		return specInput{}, fmt.Errorf("the schema of %s is of type %q, want an object whose properties are its arguments",
			spec.Name, schema.Type)
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	// A tool that requires nothing still has to say so: the list travels to the
	// provider as it is, and a null where an array belongs is a schema a
	// validating provider refuses.
	if schema.Required == nil {
		schema.Required = []string{}
	}
	return specInput{properties: schema.Properties, required: schema.Required}, nil
}

// callHandler forwards a session tool call to the spec's handler. A returned
// error becomes a text tool response, so the model reads the failure and can
// correct itself within the same turn.
func callHandler(spec mcpservers.Spec, onToolCall func(string)) func(context.Context, map[string]any) (kit.ToolOutput, error) {
	return func(ctx context.Context, args map[string]any) (kit.ToolOutput, error) {
		if onToolCall != nil {
			onToolCall(spec.Name)
		}
		text, err := spec.Handle(ctx, args)
		if err != nil {
			return kit.ToolOutput{}, err
		}
		return kit.TextResult(text), nil
	}
}
