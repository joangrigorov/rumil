package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// mcpBase is the smallest valid configuration, plus whatever MCP block a test
// needs.
const mcpBase = `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
`

// An omitted transport is stdio, and the two caps fall back to their defaults.
func TestMCPServerDefaults(t *testing.T) {
	cfg := mustParse(t, mcpBase+`
mcp_servers:
  - name: "workspace"
    command: "rumil-workspace"
    tools:
      allow: ["run_tests"]
`)

	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcp_servers = %d, want one", len(cfg.MCPServers))
	}
	server := cfg.MCPServers[0]
	if server.Transport != "stdio" {
		t.Errorf("transport = %q, want stdio", server.Transport)
	}
	if got := server.TimeoutOrDefault(); got != DefaultMCPTimeout {
		t.Errorf("timeout = %s, want %s", got, DefaultMCPTimeout)
	}
	if got := server.MaxOutputOrDefault(); got != DefaultMCPMaxOutputBytes {
		t.Errorf("max_output_bytes = %d, want %d", got, DefaultMCPMaxOutputBytes)
	}
}

// Both transports accept the fields that belong to them, and the configured
// caps replace the defaults.
func TestMCPServersAcceptBothTransports(t *testing.T) {
	cfg := mustParse(t, mcpBase+`
mcp_servers:
  - name: "workspace"
    transport: "stdio"
    command: "rumil-workspace"
    args: ["--root", "/srv/repo"]
    env:
      WORKSPACE_TOKEN: "token"
    timeout: "90s"
    max_output_bytes: 8192
    tools:
      allow: ["run_tests"]
  - name: "docs"
    transport: "http"
    url: "https://mcp.example.com/mcp"
    headers:
      Authorization: "Bearer ${DOCS_TOKEN}"
    tools:
      allow: ["search_docs"]
`)

	if len(cfg.MCPServers) != 2 {
		t.Fatalf("mcp_servers = %d, want two", len(cfg.MCPServers))
	}
	first := cfg.MCPServers[0]
	if got := first.TimeoutOrDefault(); got != 90*time.Second {
		t.Errorf("timeout = %s, want 90s", got)
	}
	if got := first.MaxOutputOrDefault(); got != 8192 {
		t.Errorf("max_output_bytes = %d, want 8192", got)
	}
	if len(first.Args) != 2 || first.Args[0] != "--root" {
		t.Errorf("args = %v, want the configured pair", first.Args)
	}
	if second := cfg.MCPServers[1]; second.URL != "https://mcp.example.com/mcp" {
		t.Errorf("url = %q, want the configured address", second.URL)
	}
}

// Every invalid server is refused at load time, with an error that names the
// field, rather than failing on the first review.
func TestMCPServersRejectInvalidConfiguration(t *testing.T) {
	tests := map[string]struct {
		block string
		want  string
	}{
		"missing name": {
			block: "  - command: \"server\"\n",
			want:  "name",
		},
		"name with a capital": {
			block: "  - name: \"Workspace\"\n    command: \"server\"\n",
			want:  "name",
		},
		"duplicate names": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n  - name: \"workspace\"\n    command: \"server\"\n",
			want:  "two servers named",
		},
		"stdio without a command": {
			block: "  - name: \"workspace\"\n    transport: \"stdio\"\n",
			want:  "needs a command",
		},
		"stdio with a url": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n    url: \"https://mcp.example.com\"\n",
			want:  "cannot have a url",
		},
		"http without a url": {
			block: "  - name: \"docs\"\n    transport: \"http\"\n",
			want:  "needs a url",
		},
		"http with a command": {
			block: "  - name: \"docs\"\n    transport: \"http\"\n    url: \"https://mcp.example.com\"\n    command: \"server\"\n",
			want:  "cannot have a command",
		},
		"http with a relative url": {
			block: "  - name: \"docs\"\n    transport: \"http\"\n    url: \"/mcp\"\n",
			want:  "absolute http or https URL",
		},
		"unknown transport": {
			block: "  - name: \"docs\"\n    transport: \"grpc\"\n",
			want:  "stdio or http",
		},
		"invalid timeout": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n    timeout: \"soon\"\n",
			want:  "timeout",
		},
		"negative timeout": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n    timeout: \"-5s\"\n",
			want:  "timeout",
		},
		"negative output cap": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n    max_output_bytes: -1\n",
			want:  "max_output_bytes",
		},
		"output cap above the maximum": {
			block: fmt.Sprintf("  - name: \"workspace\"\n    command: \"server\"\n    max_output_bytes: %d\n", MaxMCPMaxOutputBytes+1),
			want:  "max_output_bytes",
		},
		"empty allow entry": {
			block: "  - name: \"workspace\"\n    command: \"server\"\n    tools:\n      allow: [\" \"]\n",
			want:  "empty tool name",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(mcpBase+"mcp_servers:\n"+test.block), "test.yaml")
			if err == nil {
				t.Fatalf("Parse accepted a server with %s", name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("Parse = %v, want it to name %q", err, test.want)
			}
		})
	}
}

// The server count is bounded because every advertised tool costs prompt budget
// on every request.
func TestMCPServersRejectTooMany(t *testing.T) {
	block := ""
	for i := 0; i <= maxMCPServers; i++ {
		block += fmt.Sprintf("  - name: \"server-%d\"\n    command: \"server\"\n", i)
	}

	_, err := Parse([]byte(mcpBase+"mcp_servers:\n"+block), "test.yaml")
	if err == nil {
		t.Fatal("Parse accepted more servers than the limit")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("Parse = %v, want the limit named", err)
	}
}
