package hgctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"

	"github.com/x2x3studio/hgctl/internal/config"
)

const basicMemoryMCPName = "hourglass-memory"

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
	state, err := config.LoadState(a.Paths)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.BasicMemoryMCP == nil {
		state.BasicMemoryMCP = make(map[string]string)
	}
	if err := a.installMemoryService(ctx, binary); err != nil {
		return fmt.Errorf("shared Basic Memory service: %w", err)
	}
	return a.setupMemoryMCPClients(ctx, binary, state, mcpClients())
}

func (a *App) setupMemoryMCPClients(ctx context.Context, binary string, state config.State, clients []mcpClient) error {
	var configured int
	for _, client := range clients {
		if !proc.Exists(client.executable) {
			continue
		}
		exists, matches, err := inspectBasicMemoryMCP(ctx, client, memoryServiceURL)
		if err != nil {
			return fmt.Errorf("inspect %s Basic Memory MCP: %w", client.name, err)
		}
		managedPath := state.BasicMemoryMCP[client.name]
		if exists && !matches {
			legacyTarget := managedPath
			if legacyTarget == "" {
				legacyTarget = binary
			}
			_, owned, err := inspectBasicMemoryMCP(ctx, client, legacyTarget)
			if err != nil || !owned {
				return fmt.Errorf("refusing to replace modified %s Basic Memory MCP entry", client.name)
			}
			if err := removeBasicMemoryMCP(ctx, client); err != nil {
				return fmt.Errorf("re-register %s Basic Memory MCP: %w", client.name, err)
			}
			exists = false
		}
		if !exists {
			if err := addBasicMemoryMCP(ctx, client, memoryServiceURL); err != nil {
				return fmt.Errorf("configure %s Basic Memory MCP: %w", client.name, err)
			}
			managedPath = memoryServiceURL
		}
		exists, matches, err = inspectBasicMemoryMCP(ctx, client, memoryServiceURL)
		if err != nil || !exists || !matches {
			return fmt.Errorf("verify %s Basic Memory MCP: configuration is not active", client.name)
		}
		if managedPath != "" {
			state.BasicMemoryMCP[client.name] = memoryServiceURL
		}
		if err := config.SaveState(a.Paths, state); err != nil {
			return err
		}
		configured++
	}
	if configured == 0 {
		return errors.New("neither Claude Code nor Codex is installed")
	}
	return nil
}

func (a *App) repairLegacyMemoryMCP(ctx context.Context) error {
	state, err := config.LoadState(a.Paths)
	if err != nil {
		return err
	}
	for _, client := range mcpClients() {
		target := state.BasicMemoryMCP[client.name]
		if target != "" && target != memoryServiceURL && proc.Exists(client.executable) {
			return a.setupBasicMemoryMCP(ctx)
		}
	}
	return nil
}

func addBasicMemoryMCP(ctx context.Context, client mcpClient, endpoint string) error {
	args := []string{"mcp", "add", basicMemoryMCPName, "--url", endpoint}
	if client.name == "claude" {
		args = []string{"mcp", "add", "--transport", "http", "--scope", "user", basicMemoryMCPName, endpoint}
	}
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

func inspectBasicMemoryMCP(ctx context.Context, client mcpClient, target string) (bool, bool, error) {
	httpTarget := strings.HasPrefix(target, "http://")
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
				URL     string            `json:"url"`
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal([]byte(output), &entry); err != nil {
			return true, false, err
		}
		matches := entry.Enabled && entry.Transport.Type == "stdio" && entry.Transport.Command == target &&
			equalStrings(entry.Transport.Args, []string{"mcp", "--project", config.ProjectName})
		if httpTarget {
			matches = entry.Enabled && entry.Transport.Type == "streamable_http" && entry.Transport.URL == target
		}
		return true, matches, nil
	}
	output, err := proc.Run(ctx, "", client.executable, "mcp", "get", basicMemoryMCPName)
	if err != nil {
		if strings.Contains(output, "No MCP server named") {
			return false, false, nil
		}
		return false, false, err
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found {
			fields[key] = strings.TrimSpace(value)
		}
	}
	userScope := strings.HasPrefix(fields["Scope"], "User config (")
	matches := userScope && fields["Type"] == "stdio" && fields["Command"] == target && fields["Args"] == "mcp --project "+config.ProjectName
	if httpTarget {
		matches = userScope && fields["Type"] == "http" && fields["URL"] == target
	}
	return true, matches, nil
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
	state, err := config.LoadState(a.Paths)
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
			if err := config.SaveState(a.Paths, state); err != nil {
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
		if err := config.SaveState(a.Paths, state); err != nil {
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
	content, fileErr := os.ReadFile(a.memoryServicePath())
	owned := fileErr == nil && string(content) == memoryServiceDefinition(runtime.GOOS, binary, a.Paths.Home)
	serviceErr := probeMemoryService(ctx, memoryServiceURL)
	if serviceErr == nil {
		serviceErr = probeMemoryServiceProtection(ctx, memoryServiceURL)
	}
	note := memoryServiceURL
	if serviceErr != nil {
		note += ": " + fsx.Bound(serviceErr.Error(), 256)
	}
	checks := []doctorCheck{{"memory service", owned && a.memoryServiceLoaded(ctx) && serviceErr == nil, note}}
	for _, client := range mcpClients() {
		if !proc.Exists(client.executable) {
			continue
		}
		exists, matches, inspectErr := inspectBasicMemoryMCP(ctx, client, memoryServiceURL)
		note := basicMemoryMCPName
		if inspectErr != nil {
			note = fsx.Bound(inspectErr.Error(), 512)
		}
		checks = append(checks, doctorCheck{client.name + " memory MCP", inspectErr == nil && exists && matches, note})
	}
	return checks
}
