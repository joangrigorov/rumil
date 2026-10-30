package reviewer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// startMCPServer serves one in-process MCP server over streamable HTTP and
// returns the address a review's configuration points at.
func startMCPServer(t *testing.T, tools ...mcp.Tool) string {
	t.Helper()

	srv := mcpserver.NewMCPServer("test", "1")
	for _, tool := range tools {
		tool := tool
		srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			switch tool.Name {
			case "run_tests":
				return mcp.NewToolResultText("ok  github.com/acme/payments  0.4s"), nil
			case "long_report":
				return mcp.NewToolResultText(strings.Repeat("x", 4096)), nil
			case "failing_tool":
				return mcp.NewToolResultError("the workspace is not checked out"), nil
			default:
				return mcp.NewToolResultText("unexpected tool " + tool.Name), nil
			}
		})
	}

	httpServer := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithEndpointPath("/mcp")))
	t.Cleanup(httpServer.Close)
	return httpServer.URL + "/mcp"
}

func mcpTool(name string) mcp.Tool {
	return mcp.NewTool(name, mcp.WithDescription("A tool the operator allowed for the review."))
}

// An allowed tool is offered to the model, its call reaches the server, and its
// result comes back into the session; a tool the operator did not allow stays
// out of the tool set.
func TestReviewUsesAnOperatorMCPTool(t *testing.T) {
	endpoint := startMCPServer(t, mcpTool("run_tests"), mcpTool("delete_repository"))

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("mcp__workspace__run_tests", map[string]any{"package": "./..."})
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine; the tests pass.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{
		mcp: fmt.Sprintf(`
mcp_servers:
  - name: "workspace"
    transport: "http"
    url: %q
    tools:
      allow: ["run_tests"]
`, endpoint),
	})

	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	offered := llm.OfferedToolNames()
	if !contains(offered, "mcp__workspace__run_tests") {
		t.Errorf("the model was not offered the allowed tool: %v", offered)
	}
	if contains(offered, "mcp__workspace__delete_repository") {
		t.Errorf("the model was offered a tool the operator did not allow: %v", offered)
	}

	results := strings.Join(llm.ToolResults(), "\n")
	if !strings.Contains(results, "ok  github.com/acme/payments") {
		t.Errorf("the model did not receive the tool result:\n%s", results)
	}

	prompt := llm.Requests()[0].SystemPrompt
	if !strings.Contains(prompt, "operator-configured MCP server") {
		t.Errorf("the prompt does not describe the operator tools:\n%s", prompt)
	}
}

// A tool that reports an error reaches the model as a tool error rather than
// ending the review.
func TestAMCPToolErrorReachesTheModel(t *testing.T) {
	endpoint := startMCPServer(t, mcpTool("failing_tool"))

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("mcp__workspace__failing_tool", map[string]any{})
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Reviewed without the workspace.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{
		mcp: fmt.Sprintf(`
mcp_servers:
  - name: "workspace"
    transport: "http"
    url: %q
    tools:
      allow: ["failing_tool"]
`, endpoint),
	})

	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	if results := strings.Join(llm.ToolResults(), "\n"); !strings.Contains(results, "the workspace is not checked out") {
		t.Errorf("the tool error did not reach the model:\n%s", results)
	}
}

// A result larger than the cap is cut with a marker, so one call cannot fill
// the session's context.
func TestMCPOutputIsCapped(t *testing.T) {
	endpoint := startMCPServer(t, mcpTool("long_report"))

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("mcp__workspace__long_report", map[string]any{})
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Reviewed.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{
		mcp: fmt.Sprintf(`
mcp_servers:
  - name: "workspace"
    transport: "http"
    url: %q
    max_output_bytes: 256
    tools:
      allow: ["long_report"]
`, endpoint),
	})

	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	var result string
	for _, candidate := range llm.ToolResults() {
		if strings.Contains(candidate, "[output truncated]") {
			result = candidate
			break
		}
	}
	if result == "" {
		t.Fatalf("no capped result reached the model:\n%s", strings.Join(llm.ToolResults(), "\n"))
	}
	if len(result) > 300 {
		t.Errorf("the capped result is %d bytes, want it near the 256-byte cap", len(result))
	}
}

// A server that cannot be reached costs its tools, not the review.
func TestAnUnreachableMCPServerDoesNotFailTheReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{
		mcp: `
mcp_servers:
  - name: "workspace"
    transport: "http"
    url: "http://127.0.0.1:1/mcp"
    timeout: "2s"
    tools:
      allow: ["run_tests"]
`,
	})

	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "workspace") {
		t.Errorf("the logs do not name the server that failed:\n%s", h.logs.String())
	}
	for _, name := range llm.OfferedToolNames() {
		if strings.HasPrefix(name, "mcp__workspace__") {
			t.Errorf("a failed server still contributed tool %q", name)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
