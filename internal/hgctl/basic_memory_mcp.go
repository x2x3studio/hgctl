package hgctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"
)

const basicMemoryMCPName = "hourglass-memory"

// basicMemoryMCPEnv sets no crippling flags: recall uses Basic Memory's default
// full-text + semantic + wikilink-graph retrieval. Read-only is enforced by the
// client tool allowlist, not by disabling features here.
var basicMemoryMCPEnv = map[string]string{}

type mcpClient struct {
	name       string
	executable string
}

func mcpClients() []mcpClient {
	return []mcpClient{{name: "claude", executable: "claude"}, {name: "codex", executable: "codex"}}
}

func (a *App) setupBasicMemoryMCP(ctx context.Context) error {
	binary, err := exec.LookPath("basic-memory")
	if err != nil {
		return errors.New("basic-memory is not installed")
	}
	state, err := a.loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.BasicMemoryMCP == nil {
		state.BasicMemoryMCP = make(map[string]string)
	}
	var configured int
	for _, client := range mcpClients() {
		if !proc.Exists(client.executable) {
			continue
		}
		exists, matches, err := inspectBasicMemoryMCP(ctx, client, binary)
		if err != nil {
			return fmt.Errorf("inspect %s Basic Memory MCP: %w", client.name, err)
		}
		managedPath := state.BasicMemoryMCP[client.name]
		if exists && !matches {
			// Adopt an entry that differs only in managed command/args/env by
			// re-registering it rather than refusing (e.g. a hand-registered
			// entry with different transport flags).
			if err := removeBasicMemoryMCP(ctx, client); err != nil {
				return fmt.Errorf("re-register %s Basic Memory MCP: %w", client.name, err)
			}
			exists = false
		}
		if !exists {
			if err := addBasicMemoryMCP(ctx, client, binary); err != nil {
				return fmt.Errorf("configure %s Basic Memory MCP: %w", client.name, err)
			}
			managedPath = binary
		}
		exists, matches, err = inspectBasicMemoryMCP(ctx, client, binary)
		if err != nil || !exists || !matches {
			return fmt.Errorf("verify %s Basic Memory MCP: configuration is not active", client.name)
		}
		if managedPath != "" {
			state.BasicMemoryMCP[client.name] = managedPath
		}
		if err := a.saveState(state); err != nil {
			return err
		}
		configured++
	}
	if configured == 0 {
		return errors.New("neither Claude Code nor Codex is installed")
	}
	return nil
}

func addBasicMemoryMCP(ctx context.Context, client mcpClient, binary string) error {
	envKeys := make([]string, 0, len(basicMemoryMCPEnv))
	for key := range basicMemoryMCPEnv {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	var args []string
	if client.name == "claude" {
		args = []string{"mcp", "add", basicMemoryMCPName, "--scope", "user"}
		for _, key := range envKeys {
			args = append(args, "-e", key+"="+basicMemoryMCPEnv[key])
		}
	} else {
		args = []string{"mcp", "add"}
		for _, key := range envKeys {
			args = append(args, "--env", key+"="+basicMemoryMCPEnv[key])
		}
		args = append(args, basicMemoryMCPName)
	}
	args = append(args, "--", binary, "mcp", "--project", ProjectName)
	_, err := proc.Run(ctx, "", client.executable, args...)
	return err
}

func removeBasicMemoryMCP(ctx context.Context, client mcpClient) error {
	args := []string{"mcp", "remove", basicMemoryMCPName}
	if client.name == "claude" {
		args = append(args, "--scope", "user")
	}
	_, err := proc.Run(ctx, "", client.executable, args...)
	return err
}

func inspectBasicMemoryMCP(ctx context.Context, client mcpClient, binary string) (bool, bool, error) {
	if client.name == "codex" {
		output, err := proc.Run(ctx, "", client.executable, "mcp", "get", basicMemoryMCPName, "--json")
		if err != nil {
			if strings.Contains(output, "No MCP server named") {
				return false, false, nil
			}
			return false, false, err
		}
		var entry struct {
			Enabled   bool `json:"enabled"`
			Transport struct {
				Type    string            `json:"type"`
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal([]byte(output), &entry); err != nil {
			return true, false, err
		}
		matches := entry.Enabled && entry.Transport.Type == "stdio" && entry.Transport.Command == binary &&
			equalStrings(entry.Transport.Args, []string{"mcp", "--project", ProjectName}) && containsEnv(entry.Transport.Env)
		return true, matches, nil
	}
	output, err := proc.Run(ctx, "", client.executable, "mcp", "get", basicMemoryMCPName)
	if err != nil {
		if strings.Contains(output, "No MCP server named") {
			return false, false, nil
		}
		return false, false, err
	}
	matches := strings.Contains(output, "Scope: User config") &&
		strings.Contains(output, "Type: stdio") &&
		strings.Contains(output, "Command: "+binary) &&
		strings.Contains(output, "Args: mcp --project "+ProjectName)
	for key, value := range basicMemoryMCPEnv {
		matches = matches && strings.Contains(output, key+"="+value)
	}
	return true, matches, nil
}

func containsEnv(actual map[string]string) bool {
	for key, value := range basicMemoryMCPEnv {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (a *App) removeManagedBasicMemoryMCP(ctx context.Context) error {
	state, err := a.loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, client := range mcpClients() {
		managedPath := state.BasicMemoryMCP[client.name]
		if managedPath == "" || !proc.Exists(client.executable) {
			continue
		}
		exists, matches, inspectErr := inspectBasicMemoryMCP(ctx, client, managedPath)
		if inspectErr != nil || (exists && !matches) {
			return fmt.Errorf("refusing to remove modified %s Basic Memory MCP entry", client.name)
		}
		if !exists {
			delete(state.BasicMemoryMCP, client.name)
			if err := a.saveState(state); err != nil {
				return err
			}
			continue
		}
		args := []string{"mcp", "remove", basicMemoryMCPName}
		if client.name == "claude" {
			args = append(args, "--scope", "user")
		}
		if _, err := proc.Run(ctx, "", client.executable, args...); err != nil {
			return fmt.Errorf("remove %s Basic Memory MCP: %w", client.name, err)
		}
		delete(state.BasicMemoryMCP, client.name)
		if err := a.saveState(state); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) basicMemoryMCPDoctorChecks(ctx context.Context) []doctorCheck {
	binary, err := exec.LookPath("basic-memory")
	if err != nil {
		return nil
	}
	var checks []doctorCheck
	for _, client := range mcpClients() {
		if !proc.Exists(client.executable) {
			continue
		}
		exists, matches, inspectErr := inspectBasicMemoryMCP(ctx, client, binary)
		note := basicMemoryMCPName
		if inspectErr != nil {
			note = fsx.Bound(inspectErr.Error(), 512)
		}
		checks = append(checks, doctorCheck{client.name + " memory MCP", inspectErr == nil && exists && matches, note})
	}
	return checks
}
