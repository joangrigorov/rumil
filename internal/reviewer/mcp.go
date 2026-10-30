package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/snapshot"
)

// maxMCPDescriptionBytes caps an advertised tool description. The schema is
// sent on every request, so a verbose server must not fill the prompt.
const maxMCPDescriptionBytes = 1024

// maxMCPToolName is the longest name an OpenAI-compatible provider accepts.
const maxMCPToolName = 64

// mcpTools connects the operator's MCP servers and returns their allowed tools
// as session specs. The returned function closes every connection and is safe
// to call even when no server started.
func (e *Engine) mcpTools(ctx context.Context, req Request, headSHA string, snap *snapshot.Snapshot) ([]mcpservers.Spec, func()) {
	var (
		specs   []mcpservers.Spec
		closers []func()
	)
	for _, server := range e.cfg.MCPServers {
		serverSpecs, closeServer := e.mcpServerTools(ctx, req, headSHA, snap, server)
		if closeServer != nil {
			closers = append(closers, closeServer)
		}
		specs = append(specs, serverSpecs...)
	}
	return specs, func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
}

// mcpServerTools connects one server, lists its tools, and keeps only the ones
// the operator allowed. A server that fails at any point is logged and skipped:
// the review does not depend on it.
func (e *Engine) mcpServerTools(ctx context.Context, req Request, headSHA string, snap *snapshot.Snapshot, server config.MCPServer) ([]mcpservers.Spec, func()) {
	connectCtx, cancel := context.WithTimeout(ctx, server.TimeoutOrDefault())
	defer cancel()

	mcpClient, err := connectMCP(connectCtx, server, mcpEnvironment(req, headSHA, snap))
	if err != nil {
		e.logger.Warn("connecting to an MCP server failed, the review runs without its tools",
			"server", server.Name, "error", err)
		return nil, nil
	}
	closeClient := func() {
		if err := mcpClient.Close(); err != nil {
			e.logger.Warn("closing an MCP server failed", "server", server.Name, "error", err)
		}
	}

	listed, err := mcpClient.ListTools(connectCtx, mcp.ListToolsRequest{})
	if err != nil {
		e.logger.Warn("listing an MCP server's tools failed, the review runs without them",
			"server", server.Name, "error", err)
		closeClient()
		return nil, nil
	}

	allowed := make(map[string]bool, len(server.Tools.Allow))
	for _, name := range server.Tools.Allow {
		allowed[name] = true
	}
	if len(allowed) == 0 {
		e.logger.Info("an MCP server is configured with no allowed tools, so none are exposed", "server", server.Name)
		closeClient()
		return nil, nil
	}

	var specs []mcpservers.Spec
	seen := make(map[string]bool, len(listed.Tools))
	// One mutex per server serializes every call to its client.
	var callMu sync.Mutex
	for _, tool := range listed.Tools {
		if !allowed[tool.Name] {
			continue
		}
		delete(allowed, tool.Name)

		name := mcpToolName(server.Name, tool.Name)
		if seen[name] {
			e.logger.Warn("two MCP tools advertise the same name, the second is skipped",
				"server", server.Name, "tool", tool.Name, "name", name)
			continue
		}
		schema, err := mcpToolSchema(tool)
		if err != nil {
			e.logger.Warn("an MCP tool's schema could not be read, the tool is skipped",
				"server", server.Name, "tool", tool.Name, "error", err)
			continue
		}
		seen[name] = true
		specs = append(specs, mcpservers.Spec{
			Name:        name,
			Description: truncateMCPText(tool.Description, maxMCPDescriptionBytes, ""),
			Schema:      schema,
			Handle:      mcpToolHandler(mcpClient, &callMu, server, tool.Name),
		})
	}
	for missing := range allowed {
		e.logger.Warn("an allowed MCP tool was not offered by its server", "server", server.Name, "tool", missing)
	}
	if len(specs) == 0 {
		closeClient()
		return nil, nil
	}

	e.logger.Info("loaded MCP tools", "server", server.Name, "tools", len(specs))
	return specs, closeClient
}

// connectMCP starts the configured transport and initializes the session.
func connectMCP(ctx context.Context, server config.MCPServer, baseEnv []string) (*client.Client, error) {
	var (
		mcpClient *client.Client
		err       error
	)
	switch server.Transport {
	case "stdio":
		env := append(baseEnv, sortedEnvironment(server.Env)...)
		mcpClient, err = client.NewStdioMCPClient(server.Command, env, server.Args...)
	case "http":
		options := []transport.StreamableHTTPCOption{
			transport.WithHTTPTimeout(server.TimeoutOrDefault()),
		}
		if len(server.Headers) > 0 {
			options = append(options, transport.WithHTTPHeaders(server.Headers))
		}
		mcpClient, err = client.NewStreamableHttpClient(server.URL, options...)
		if err == nil {
			err = mcpClient.Start(ctx)
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q", server.Transport)
	}
	if err != nil {
		return nil, err
	}

	if _, err := mcpClient.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "rumil", Version: "1"},
	}}); err != nil {
		_ = mcpClient.Close()
		return nil, fmt.Errorf("initializing the session: %w", err)
	}
	return mcpClient, nil
}

// mcpEnvironment is what a stdio server sees about the session. The commit and
// workspace let a test-runner server act on the code under review.
func mcpEnvironment(req Request, headSHA string, snap *snapshot.Snapshot) []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"RUMIL_REPO="+req.Owner+"/"+req.Repo,
		fmt.Sprintf("RUMIL_PR=%d", req.Number),
	)
	if headSHA != "" {
		env = append(env, "RUMIL_HEAD_SHA="+headSHA)
	}
	if req.SessionID != "" {
		env = append(env, "RUMIL_SESSION_ID="+req.SessionID)
	}
	if snap != nil {
		env = append(env, "RUMIL_WORKSPACE="+snap.Dir())
	}
	return env
}

// sortedEnvironment renders configured variables in a stable order, so a
// process's environment does not depend on map iteration.
func sortedEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

// mcpToolHandler forwards one session tool call to the server. Calls are
// serialized per server because an MCP client is not documented as safe for
// concurrent use, and kit may run tool calls in parallel.
func mcpToolHandler(mcpClient *client.Client, mu *sync.Mutex, server config.MCPServer, toolName string) mcpservers.Handler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, server.TimeoutOrDefault())
		defer cancel()

		mu.Lock()
		result, err := mcpClient.CallTool(callCtx, mcp.CallToolRequest{Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		}})
		mu.Unlock()
		if err != nil {
			return "", fmt.Errorf("calling MCP tool %s: %w", toolName, err)
		}

		text := truncateMCPText(mcpResultText(result), server.MaxOutputOrDefault(), "\n[output truncated]")
		if result.IsError {
			return "", errors.New(text)
		}
		return text, nil
	}
}

// mcpResultText renders the text content of a result. A structured-only result
// is rendered as JSON, which is what a model can read.
func mcpResultText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	for _, content := range result.Content {
		text, ok := mcp.AsTextContent(content)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text.Text)
	}
	if b.Len() == 0 && result.StructuredContent != nil {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil {
			b.Write(encoded)
		}
	}
	if b.Len() == 0 {
		return "(the MCP tool returned no text)"
	}
	return b.String()
}

// mcpToolSchema is the object schema the session advertises. The protocol's
// extra JSON Schema keywords are dropped because kit forwards the properties
// and required list alone.
func mcpToolSchema(tool mcp.Tool) (json.RawMessage, error) {
	properties := tool.InputSchema.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	required := tool.InputSchema.Required
	if required == nil {
		required = []string{}
	}
	return json.Marshal(map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	})
}

// mcpToolName namespaces a server's tool so two servers cannot collide and the
// prompt can tell an operator's tool from a built-in one.
func mcpToolName(server, tool string) string {
	name := "mcp__" + server + "__" + sanitizeMCPName(tool)
	if len(name) > maxMCPToolName {
		name = name[:maxMCPToolName]
	}
	return name
}

// sanitizeMCPName keeps the characters a provider accepts in a tool name.
func sanitizeMCPName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// truncateMCPText caps text at a rune boundary and adds marker when it cut
// something. An empty marker means a silent cut, which a description uses.
func truncateMCPText(text string, max int, marker string) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + marker
}
