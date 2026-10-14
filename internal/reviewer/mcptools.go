package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcpClientName identifies Rumil to the MCP server.
const mcpClientName = "rumil"

// inProcessTools connects to the mcp-go server over the in-process transport
// and exposes its tools to a kit session as raw tools.
//
// kit's own MCP integration is not used here: its MCPToolManager reads the
// tool slice without holding the mutex that the parallel server loader writes
// under (internal/tools/mcp.go, GetTools vs loadServerTools), which the race
// detector reports as a data race in every session. Binding the same server
// through the public raw-tool API keeps the MCP tool definitions, schemas, and
// transport, and removes the unsynchronized read from the hot path.
//
// The returned stop function closes the in-process connection.
func inProcessTools(ctx context.Context, srv *server.MCPServer, namespace string) ([]kit.Tool, func(), error) {
	mcpClient, err := client.NewInProcessClient(srv)
	if err != nil {
		return nil, nil, fmt.Errorf("creating the in-process MCP client: %w", err)
	}

	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := mcpClient.Start(startCtx); err != nil {
		_ = mcpClient.Close()
		return nil, nil, fmt.Errorf("starting the in-process MCP client: %w", err)
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: mcpClientName, Version: "v0.0.1"}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}
	if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
		_ = mcpClient.Close()
		return nil, nil, fmt.Errorf("initializing the in-process MCP client: %w", err)
	}

	listed, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = mcpClient.Close()
		return nil, nil, fmt.Errorf("listing MCP tools: %w", err)
	}

	tools := make([]kit.Tool, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		schema, err := schemaMap(tool)
		if err != nil {
			_ = mcpClient.Close()
			return nil, nil, err
		}
		name := tool.Name
		if namespace != "" {
			name = namespace + "__" + tool.Name
		}
		tools = append(tools, kit.NewRawTool(name, tool.Description, schema, callTool(mcpClient, tool.Name)))
	}

	return tools, func() { _ = mcpClient.Close() }, nil
}

// schemaMap returns the tool's input schema as the map kit expects.
func schemaMap(tool mcp.Tool) (map[string]any, error) {
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("encoding the schema of %s: %w", tool.Name, err)
	}
	schema := map[string]any{}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return nil, fmt.Errorf("decoding the schema of %s: %w", tool.Name, err)
	}
	return schema, nil
}

// callTool forwards a session tool call to the MCP server.
func callTool(mcpClient *client.Client, name string) func(context.Context, map[string]any) (kit.ToolOutput, error) {
	return func(ctx context.Context, args map[string]any) (kit.ToolOutput, error) {
		request := mcp.CallToolRequest{}
		request.Params.Name = name
		request.Params.Arguments = args

		result, err := mcpClient.CallTool(ctx, request)
		if err != nil {
			return kit.ToolOutput{}, fmt.Errorf("calling %s: %w", name, err)
		}
		return kit.TextResult(toolResultText(result)), nil
	}
}

// toolResultText flattens the text content of an MCP tool result.
func toolResultText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}

	var text string
	for _, content := range result.Content {
		if part, ok := content.(mcp.TextContent); ok {
			if text != "" {
				text += "\n"
			}
			text += part.Text
		}
	}
	return text
}
