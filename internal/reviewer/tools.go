package reviewer

import (
	"context"
	"encoding/json"
	"fmt"

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
		schema, err := schemaMap(spec)
		if err != nil {
			return nil, err
		}
		tools = append(tools, kit.NewRawTool(spec.Name, spec.Description, schema, callHandler(spec, onToolCall)))
	}
	return tools, nil
}

// schemaMap returns the spec's JSON Schema as the decoded map kit advertises to
// the model.
func schemaMap(spec mcpservers.Spec) (map[string]any, error) {
	schema := map[string]any{}
	if err := json.Unmarshal(spec.Schema, &schema); err != nil {
		return nil, fmt.Errorf("decoding the schema of %s: %w", spec.Name, err)
	}
	return schema, nil
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
