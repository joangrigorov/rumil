package mcpservers_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/joangrigorov/rumil/internal/mcpservers"
)

// TestSpecsSurviveInProcessRoundTrip pins the tool surface the model sees: the
// names, and the JSON schema that travels over the in-process MCP transport.
func TestSpecsSurviveInProcessRoundTrip(t *testing.T) {
	tools := mcpservers.NewGithubTools(nil, 4096, slog.New(slog.DiscardHandler))
	srv := tools.Server("v0.0.1-test")

	listed := listTools(t, srv)

	wantNames := []string{
		"get_pr_metadata",
		"get_pr_diff",
		"get_file_contents",
		"list_existing_review_comments",
		"submit_pr_review",
	}
	got := make(map[string]mcp.Tool, len(listed))
	for _, tool := range listed {
		got[tool.Name] = tool
	}
	for _, name := range wantNames {
		if _, ok := got[name]; !ok {
			t.Errorf("server does not expose %q", name)
		}
	}
	if len(listed) != len(wantNames) {
		t.Errorf("server exposes %d tools, want %d", len(listed), len(wantNames))
	}

	raw, err := json.Marshal(got["submit_pr_review"])
	if err != nil {
		t.Fatalf("marshaling the tool: %v", err)
	}
	schema := string(raw)
	t.Logf("submit_pr_review as seen by a client: %s", schema)

	for _, want := range []string{"REQUEST_CHANGES", "APPROVE", "summary_body", "comments", "required"} {
		if !strings.Contains(schema, want) {
			t.Errorf("submit_pr_review schema lost %q: %s", want, schema)
		}
	}

	raw, err = json.Marshal(got["get_file_contents"])
	if err != nil {
		t.Fatalf("marshaling the tool: %v", err)
	}
	if !strings.Contains(string(raw), "file_path") {
		t.Errorf("get_file_contents schema lost file_path: %s", raw)
	}
}

func listTools(t *testing.T, srv *server.MCPServer) []mcp.Tool {
	t.Helper()

	mcpClient, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("creating the in-process client: %v", err)
	}
	t.Cleanup(func() { _ = mcpClient.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("starting the in-process client: %v", err)
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "test"}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}
	if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
		t.Fatalf("initializing the in-process client: %v", err)
	}

	listed, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	return listed.Tools
}
