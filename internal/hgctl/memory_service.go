package hgctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"
)

const (
	memoryServiceLabel     = "com.x2x3studio.hgctl.memory"
	memoryServiceOwnership = "x2x3studio.hgctl.memory/v1"
	memoryServiceAddress   = "127.0.0.1:18765"
	memoryServiceURL       = "http://" + memoryServiceAddress + "/mcp"
)

func (a *App) memoryServicePath() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(a.Paths.Home, "Library", "LaunchAgents", memoryServiceLabel+".plist")
	}
	return filepath.Join(a.Paths.Home, ".config", "systemd", "user", memoryServiceLabel+".service")
}

func memoryServiceDefinition(goos, binary, home string) string {
	if goos == "darwin" {
		e := html.EscapeString
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + memoryServiceLabel + `</string>
<key>ProgramArguments</key><array>
<string>` + e(binary) + `</string><string>mcp</string>
<string>--project</string><string>hourglass</string>
<string>--transport</string><string>streamable-http</string>
<string>--host</string><string>127.0.0.1</string>
<string>--port</string><string>18765</string>
<string>--path</string><string>/mcp</string>
</array>
<key>EnvironmentVariables</key><dict>
<key>HOME</key><string>` + e(home) + `</string>
<key>HGCTL_MEMORY_OWNERSHIP</key><string>` + memoryServiceOwnership + `</string>
<key>FASTMCP_HTTP_HOST_ORIGIN_PROTECTION</key><string>true</string>
<key>FASTMCP_HTTP_SESSION_IDLE_TIMEOUT</key><string>3600</string>
</dict>
<key>WorkingDirectory</key><string>` + e(home) + `</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>10</integer>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>/dev/null</string>
</dict></plist>
`
	}
	return fmt.Sprintf("# X-HGCTL-Ownership=%s\n[Unit]\nDescription=Hourglass shared local memory MCP\n\n[Service]\nType=simple\nEnvironment=\"HOME=%s\"\nEnvironment=\"FASTMCP_HTTP_HOST_ORIGIN_PROTECTION=true\"\nEnvironment=\"FASTMCP_HTTP_SESSION_IDLE_TIMEOUT=3600\"\nWorkingDirectory=%s\nExecStart=%s mcp --project hourglass --transport streamable-http --host 127.0.0.1 --port 18765 --path /mcp\nRestart=always\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n", memoryServiceOwnership, systemdQuote(home), systemdEscape(home), systemdEscape(binary))
}

func (a *App) verifyMemoryServiceOwnership() error {
	path := a.memoryServicePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing unmanaged memory service path %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(content)
	marker := "# X-HGCTL-Ownership=" + memoryServiceOwnership + "\n"
	if runtime.GOOS == "darwin" {
		marker = "<key>HGCTL_MEMORY_OWNERSHIP</key><string>" + memoryServiceOwnership + "</string>"
	}
	if !strings.Contains(text, marker) {
		return fmt.Errorf("refusing unmanaged memory service file %s", path)
	}
	return nil
}

func (a *App) memoryServiceLoaded(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		_, err := proc.Run(ctx, "", "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+memoryServiceLabel)
		return err == nil
	}
	if runtime.GOOS == "linux" {
		_, err := proc.Run(ctx, "", "systemctl", "--user", "is-active", "--quiet", memoryServiceLabel+".service")
		return err == nil
	}
	return false
}

func (a *App) installMemoryService(ctx context.Context, binary string) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("shared memory MCP is unsupported on %s", runtime.GOOS)
	}
	if err := a.verifyMemoryServiceOwnership(); err != nil {
		return err
	}
	repaired, err := a.repairBasicMemoryVectorCleanup(ctx)
	if err != nil {
		return err
	}
	path := a.memoryServicePath()
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	loaded := a.memoryServiceLoaded(ctx)
	if loaded && len(previous) == 0 {
		return errors.New("memory service is loaded without an owned definition")
	}
	if !loaded {
		conn, err := net.DialTimeout("tcp", memoryServiceAddress, time.Second)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("memory MCP port %s is already occupied by an unmanaged service", memoryServiceAddress)
		}
	}
	desired := []byte(memoryServiceDefinition(runtime.GOOS, binary, a.Paths.Home))
	changed := !bytes.Equal(previous, desired)
	if runtime.GOOS == "linux" {
		if err := ensureUserLinger(ctx); err != nil {
			return err
		}
	}
	if changed {
		if err := fsx.WriteAtomic(path, desired, 0o644); err != nil {
			return err
		}
	}
	if changed || repaired || !loaded {
		if runtime.GOOS == "darwin" {
			domain := "gui/" + strconv.Itoa(os.Getuid())
			if loaded {
				if _, err := proc.Run(ctx, "", "launchctl", "bootout", domain+"/"+memoryServiceLabel); err != nil {
					return err
				}
			}
			if err := bootstrapMemoryService(ctx, domain, path); err != nil {
				return err
			}
		} else {
			if _, err := proc.Run(ctx, "", "systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			if _, err := proc.Run(ctx, "", "systemctl", "--user", "enable", "--now", memoryServiceLabel+".service"); err != nil {
				return err
			}
			if (changed || repaired) && loaded {
				if _, err := proc.Run(ctx, "", "systemctl", "--user", "restart", memoryServiceLabel+".service"); err != nil {
					return err
				}
			}
		}
	}
	readyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		err := probeMemoryService(readyCtx, memoryServiceURL)
		if err == nil {
			if err := probeMemoryServiceProtection(readyCtx, memoryServiceURL); err != nil {
				return errors.Join(err, a.removeMemoryService(ctx))
			}
			return nil
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("shared memory MCP did not become ready: %w", err)
		case <-time.After(time.Second):
		}
	}
}

func bootstrapMemoryService(ctx context.Context, domain, path string) error {
	// launchd may still be retiring a booted-out service after bootout returns.
	for attempt := 0; ; attempt++ {
		_, err := proc.Run(ctx, "", "launchctl", "bootstrap", domain, path)
		if err == nil || attempt >= 9 || !strings.Contains(strings.ToLower(proc.FailureOutput(err)), "input/output error") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (a *App) removeMemoryService(ctx context.Context) error {
	if err := a.verifyMemoryServiceOwnership(); err != nil {
		return err
	}
	path := a.memoryServicePath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if a.memoryServiceLoaded(ctx) {
			return errors.New("refusing memory service without an owned definition")
		}
		return nil
	} else if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		_, err := proc.Run(ctx, "", "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+memoryServiceLabel)
		if err != nil && !ignorableSchedulerStopError(err) {
			return err
		}
	} else {
		_, err := proc.Run(ctx, "", "systemctl", "--user", "disable", "--now", memoryServiceLabel+".service")
		if err != nil && !ignorableSchedulerStopError(err) {
			return err
		}
		inactive, err := systemdUnitInactive(ctx, memoryServiceLabel+".service")
		if err != nil || !inactive {
			return fmt.Errorf("memory service did not stop: inactive=%v error=%v", inactive, err)
		}
	}
	if a.memoryServiceLoaded(ctx) {
		return errors.New("memory service is still loaded")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		_, err := proc.Run(ctx, "", "systemctl", "--user", "daemon-reload")
		return err
	}
	return nil
}

func probeMemoryServiceProtection(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	for _, header := range []string{"Origin", "Host"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if header == "Origin" {
			req.Header.Set(header, "https://untrusted.invalid")
		} else {
			req.Host = "untrusted.invalid"
		}
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusMisdirectedRequest {
			return fmt.Errorf("memory MCP does not reject untrusted %s (HTTP %d); upgrade Basic Memory to a version with FastMCP HTTP host/origin protection, then rerun hgctl mcp", header, response.StatusCode)
		}
	}
	return nil
}

type memoryRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func probeMemoryService(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	session, protocol := "", "2025-03-26"
	request := func(method string, payload any) (*http.Response, error) {
		var body io.Reader
		if payload != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", protocol)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		return client.Do(req)
	}
	response, err := request(http.MethodPost, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "hgctl-doctor", "version": Version}},
	})
	if err != nil {
		return fmt.Errorf("connect local memory MCP: %w", err)
	}
	session = response.Header.Get("Mcp-Session-Id")
	if session != "" {
		defer func() {
			if response, err := request(http.MethodDelete, nil); err == nil {
				_ = response.Body.Close()
			}
		}()
	}
	payload, err := readMemoryRPC(response, 1)
	if err != nil {
		return err
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(payload, &initialized); err != nil {
		return err
	}
	if initialized.ServerInfo.Name != "Basic Memory" || initialized.ProtocolVersion == "" {
		return errors.New("local MCP endpoint is not Basic Memory")
	}
	protocol = initialized.ProtocolVersion
	response, err = request(http.MethodPost, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("MCP initialized notification: HTTP %d", response.StatusCode)
	}
	response, err = request(http.MethodPost, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if err != nil {
		return err
	}
	payload, err = readMemoryRPC(response, 2)
	if err != nil {
		return err
	}
	var listing struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return err
	}
	found := map[string]bool{}
	for _, tool := range listing.Tools {
		found[tool.Name] = true
	}
	if !found["search_notes"] || !found["read_note"] {
		return errors.New("local memory MCP is missing recall tools")
	}
	return nil
}

func readMemoryRPC(response *http.Response, id int) (json.RawMessage, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory MCP: HTTP %d", response.StatusCode)
	}
	const limit = 4 << 20
	reader := io.LimitReader(response.Body, limit+1)
	decode := func(data []byte) (json.RawMessage, bool, error) {
		var rpc memoryRPCResponse
		if err := json.Unmarshal(data, &rpc); err != nil {
			return nil, false, err
		}
		if rpc.JSONRPC != "2.0" {
			return nil, false, errors.New("invalid memory MCP response")
		}
		if rpc.ID != id {
			return nil, false, nil
		}
		if len(rpc.Error) != 0 && string(rpc.Error) != "null" {
			return nil, false, errors.New("memory MCP returned an RPC error")
		}
		if len(rpc.Result) == 0 {
			return nil, false, errors.New("memory MCP returned no result")
		}
		return rpc.Result, true, nil
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), limit)
		var event []byte
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				event = append(event, []byte(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")+"\n")...)
			}
			if line == "" && len(event) > 0 {
				result, found, err := decode(event)
				if err != nil || found {
					return result, err
				}
				event = nil
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("memory MCP stream ended before its response")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("memory MCP response exceeds limit")
	}
	result, found, err := decode(data)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("memory MCP response id mismatch")
	}
	return result, nil
}
