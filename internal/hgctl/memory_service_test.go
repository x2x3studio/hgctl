package hgctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/x2x3studio/hgctl/internal/config"
)

func TestMemoryServiceRejectsUntrustedHostAndOrigin(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if guarded && (r.Host == "untrusted.invalid" || r.Header.Get("Origin") == "https://untrusted.invalid") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
		}))
		err := probeMemoryServiceProtection(context.Background(), server.URL)
		server.Close()
		if (err == nil) != guarded {
			t.Fatalf("guarded=%v error=%v", guarded, err)
		}
	}
}

func TestMemoryServiceDefinitionsBindOnlyLoopback(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		body := memoryServiceDefinition(goos, "/home/a b/basic-memory", "/home/a b")
		for _, required := range []string{memoryServiceOwnership, "streamable-http", "127.0.0.1", "18765", "hourglass", "FASTMCP_HTTP_HOST_ORIGIN_PROTECTION", "FASTMCP_HTTP_SESSION_IDLE_TIMEOUT"} {
			if !strings.Contains(body, required) {
				t.Fatalf("%s lacks %q", goos, required)
			}
		}
		if strings.Contains(body, "0.0.0.0") {
			t.Fatalf("%s exposes recall to the network", goos)
		}
	}
	body := memoryServiceDefinition("darwin", "/home/a&b/basic-memory", "/home/a&b")
	if !strings.Contains(body, "a&amp;b") {
		t.Fatal("plist values are not escaped")
	}
}

func TestMemoryServiceOwnership(t *testing.T) {
	app := testApp(t)
	path := app.memoryServicePath()
	if err := app.verifyMemoryServiceOwnership(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{"unrelated service", false},
		{memoryServiceDefinition(runtime.GOOS, "/bin/basic-memory", app.Paths.Home), true},
		{strings.ReplaceAll(memoryServiceDefinition(runtime.GOOS, "/bin/basic-memory", app.Paths.Home), memoryServiceOwnership, "x2x3studio.hgctl.memory/v2"), false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := app.verifyMemoryServiceOwnership()
		if (err == nil) != tc.valid {
			t.Fatalf("valid=%v error=%v", tc.valid, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", path); err != nil {
		t.Fatal(err)
	}
	if err := app.verifyMemoryServiceOwnership(); err == nil {
		t.Fatal("adopted a symlink")
	}
}

func TestProbeMemoryServiceHandshake(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var mu sync.Mutex
			var methods []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodDelete {
					methods = append(methods, "DELETE")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var rpc struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				methods = append(methods, rpc.Method)
				var result any
				switch rpc.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "test-session")
					result = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "Basic Memory"}}
				case "notifications/initialized":
					if r.Header.Get("Mcp-Session-Id") != "test-session" {
						t.Error("missing session")
					}
					w.WriteHeader(http.StatusAccepted)
					return
				case "tools/list":
					if r.Header.Get("Mcp-Session-Id") != "test-session" {
						t.Error("missing session")
					}
					result = map[string]any{"tools": []map[string]string{{"name": "read_note"}, {"name": "search_notes"}}}
				default:
					t.Errorf("unexpected method %q", rpc.Method)
				}
				payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(payload)
				}
			}))
			defer srv.Close()
			if err := probeMemoryService(context.Background(), srv.URL); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list,DELETE" {
				t.Fatal(methods)
			}
		})
	}
}

func TestProbeMemoryServiceRejectsUnrelatedOrBrokenServer(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"Other MCP"}}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`not json`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
		if err := probeMemoryService(context.Background(), srv.URL); err == nil {
			t.Error("accepted", body)
		}
		srv.Close()
	}
}

func TestMemoryMCPRegistrationUsesHTTP(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		dir := t.TempDir()
		executable := filepath.Join(dir, name)
		output := filepath.Join(dir, "args")
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+output+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := addBasicMemoryMCP(testContext(t), mcpClient{name: name, executable: executable}, memoryServiceURL); err != nil {
			t.Fatal(err)
		}
		args, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(args), memoryServiceURL) || strings.Contains(string(args), "--project") {
			t.Fatalf("wrong arguments: %s", args)
		}
		if name == "claude" && !strings.Contains(string(args), "--transport\nhttp\n") {
			t.Fatalf("wrong Claude transport: %s", args)
		}
		if name == "codex" && !strings.Contains(string(args), "--url\n") {
			t.Fatalf("wrong Codex transport: %s", args)
		}
	}
}

func TestInspectHTTPMemoryMCPRequiresExactURL(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		body := "hourglass-memory:\n  Scope: User config (available in all your projects)\n  Type: http\n  URL: " + memoryServiceURL + "\n"
		if name == "codex" {
			body = `{"enabled":true,"transport":{"type":"streamable_http","url":"` + memoryServiceURL + `"}}`
		}
		executable := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(executable, []byte("#!/bin/sh\ncat <<'EOF'\n"+body+"\nEOF\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		client := mcpClient{name: name, executable: executable}
		if exists, matches, err := inspectBasicMemoryMCP(testContext(t), client, memoryServiceURL); err != nil || !exists || !matches {
			t.Fatalf("%s: %v %v %v", name, exists, matches, err)
		}
		if _, matches, err := inspectBasicMemoryMCP(testContext(t), client, memoryServiceURL+"-other"); err != nil || matches {
			t.Fatalf("%s accepted another URL", name)
		}
	}
}

func TestLegacyMCPMigrationIsIdempotentAndRefusesModifiedEntries(t *testing.T) {
	for _, modified := range []bool{false, true} {
		t.Run(fmt.Sprint(modified), func(t *testing.T) {
			app := testApp(t)
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			marker := filepath.Join(dir, "http")
			log := filepath.Join(dir, "writes")
			binary := "/opt/hourglass/basic-memory"
			command := binary
			if modified {
				command += "-other"
			}
			script := fmt.Sprintf(`#!/bin/sh
case "$2" in
 get)
  printf 'hourglass-memory:\n  Scope: User config (available in all your projects)\n'
  if [ -f '%s' ]; then
   printf '  Type: http\n  URL: %s\n'
  else
   printf '  Type: stdio\n  Command: %s\n  Args: mcp --project hourglass\n'
  fi ;;
 remove) printf 'remove\n' >> '%s' ;;
 add) touch '%s'; printf 'add\n' >> '%s' ;;
esac
`, marker, memoryServiceURL, command, log, marker, log)
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			state := config.State{BasicMemoryMCP: map[string]string{"claude": binary}}
			clients := []mcpClient{{name: "claude", executable: executable}}
			err := app.setupMemoryMCPClients(testContext(t), binary, state, clients)
			if modified {
				if err == nil {
					t.Fatal("replaced a modified entry")
				}
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatal("modified entry was written")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := config.LoadState(app.Paths)
			if err != nil {
				t.Fatal(err)
			}
			if saved.BasicMemoryMCP["claude"] != memoryServiceURL {
				t.Fatal(saved)
			}
			if err := app.setupMemoryMCPClients(testContext(t), binary, saved, clients); err != nil {
				t.Fatal(err)
			}
			writes, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if string(writes) != "remove\nadd\n" {
				t.Fatalf("migration churned: %s", writes)
			}
		})
	}
}
