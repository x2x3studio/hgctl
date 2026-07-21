package hgctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectBasicMemoryMCPRequiresOwnedConfiguration(t *testing.T) {
	binary := "/opt/hourglass/basic-memory"
	tests := []struct {
		name   string
		client string
		body   string
	}{
		{
			name:   "claude",
			client: "claude",
			body: `hourglass-memory:
  Scope: User config (available in all your projects)
  Status: Connected
  Type: stdio
  Command: /opt/hourglass/basic-memory
  Args: mcp --project hourglass
  Environment:
    BASIC_MEMORY_DEFAULT_SEARCH_TYPE=text
    BASIC_MEMORY_DISABLE_PERMALINKS=true
    BASIC_MEMORY_ENSURE_FRONTMATTER_ON_SYNC=false
    BASIC_MEMORY_SEMANTIC_SEARCH_ENABLED=false
`,
		},
		{
			name:   "codex",
			client: "codex",
			body:   `{"enabled":true,"transport":{"type":"stdio","command":"/opt/hourglass/basic-memory","args":["mcp","--project","hourglass"],"env":{"BASIC_MEMORY_DEFAULT_SEARCH_TYPE":"text","BASIC_MEMORY_DISABLE_PERMALINKS":"true","BASIC_MEMORY_ENSURE_FRONTMATTER_ON_SYNC":"false","BASIC_MEMORY_SEMANTIC_SEARCH_ENABLED":"false"}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, test.client)
			script := "#!/bin/sh\ncat <<'EOF'\n" + test.body + "\nEOF\n"
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			exists, matches, err := inspectBasicMemoryMCP(testContext(t), mcpClient{name: test.client, executable: executable}, binary)
			if err != nil || !exists || !matches {
				t.Fatalf("exists=%v matches=%v err=%v", exists, matches, err)
			}
			exists, matches, err = inspectBasicMemoryMCP(testContext(t), mcpClient{name: test.client, executable: executable}, binary+"-other")
			if err != nil || !exists || matches {
				t.Fatalf("unmanaged configuration accepted: exists=%v matches=%v err=%v", exists, matches, err)
			}
		})
	}
}

func TestInspectBasicMemoryMCPRecognizesMissingEntry(t *testing.T) {
	for _, client := range []string{"claude", "codex"} {
		t.Run(client, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), client)
			script := "#!/bin/sh\necho 'No MCP server named hourglass-memory' >&2\nexit 1\n"
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			exists, matches, err := inspectBasicMemoryMCP(testContext(t), mcpClient{name: client, executable: executable}, "/bin/basic-memory")
			if err != nil || exists || matches {
				t.Fatalf("exists=%v matches=%v err=%v", exists, matches, err)
			}
		})
	}
}
