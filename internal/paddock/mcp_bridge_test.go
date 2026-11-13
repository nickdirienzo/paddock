package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func modernPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python3.13", "python3.12", "python3.11"} {
		path, err := exec.LookPath(name)
		if err == nil && exec.Command(path, "-c", "import tomllib").Run() == nil {
			return path
		}
	}
	t.Skip("sandbox installer requires Python 3.11 or newer, as provided by the image")
	return ""
}

func TestSandboxMCPInstallPreservesConfigAndRemovesRevokedEntries(t *testing.T) {
	python := modernPython(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "bridge")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("sandbox_mode = \"danger-full-access\"\n[mcp_servers.unmanaged]\ncommand = \"local-command\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".claude.json"), []byte(`{"mcpServers":{"oshell-native":{"command":"/usr/local/bin/native-mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	installer := strings.ReplaceAll(strings.ReplaceAll(installSandboxMCP, "/sandbox/.paddock", root), "/home/sandbox", home)
	for _, names := range [][]string{{"observe", "metabase"}, {"metabase"}, {}} {
		payload, err := sandboxMCPConfiguration(names)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := exec.Command(python, "-c", installer, string(payload), sandboxMCPClient).CombinedOutput(); err != nil {
			t.Fatalf("install: %s %v", b, err)
		}
		b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Sandbox string                    `toml:"sandbox_mode"`
			Servers map[string]map[string]any `toml:"mcp_servers"`
		}
		if err = toml.Unmarshal(b, &config); err != nil {
			t.Fatal(err)
		}
		if config.Sandbox != "danger-full-access" || config.Servers["unmanaged"]["command"] != "local-command" || len(config.Servers) != len(names)+1 {
			t.Fatal("overwrote unrelated config or retained revoked entries")
		}
		for _, name := range names {
			if config.Servers[name]["command"] != "/usr/bin/python3" {
				t.Fatal("incorrect sandbox MCP command")
			}
		}
		b, err = os.ReadFile(filepath.Join(home, ".claude", ".claude.json"))
		if err != nil {
			t.Fatal(err)
		}
		var claude struct {
			Servers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(b, &claude) != nil || len(claude.Servers) != len(names)+1 {
			t.Fatal("Claude MCP config did not follow trust or preserve the OpenShell server")
		}
		if native, ok := claude.Servers["oshell-native"].(map[string]any); !ok || native["command"] != "/usr/local/bin/native-mcp" {
			t.Fatal("OpenShell MCP server was changed")
		}
	}
}

type catalogMCPNative struct{}

func (catalogMCPNative) call(_ context.Context, method string, params any, result any) error {
	if method != "mcpServerStatus/list" {
		return io.ErrUnexpectedEOF
	}
	name := params.(map[string]any)["serverName"].(string)
	var server any
	switch name {
	case "observe":
		server = map[string]any{"name": name, "tools": map[string]any{"search": map[string]any{"name": "search"}}}
	case "metabase":
		server = map[string]any{"name": name, "toolsError": "login required"}
	case "empty":
		server = map[string]any{"name": name, "tools": map[string]any{}}
	default:
		server = map[string]any{"name": name}
	}
	b, _ := json.Marshal(map[string]any{"data": []any{server}})
	return json.Unmarshal(b, result)
}

func TestReadyTrustedMCPsOmitsOnlyUnavailableCatalogs(t *testing.T) {
	broker := mcpBroker{native: catalogMCPNative{}, thread: "thread"}
	ready, unavailable := readyTrustedMCPs(context.Background(), broker, []string{"observe", "metabase", "empty", "pending"})
	if strings.Join(ready, ",") != "observe,empty" || strings.Join(unavailable, ",") != "metabase,pending" {
		t.Fatalf("ready=%v unavailable=%v", ready, unavailable)
	}
}

func TestSandboxMCPRelayMultiplexesAndStopsOnHostDisconnect(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "pd-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	file := filepath.Join(root, "mcp.py")
	if err = os.WriteFile(file, []byte(sandboxMCPClient), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, python, "-u", file, "serve", root)
	input, _ := server.StdinPipe()
	output, _ := server.StdoutPipe()
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = server.Process.Kill(); _ = server.Wait() }()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != `{"ready": true}` {
		t.Fatal("relay did not become ready")
	}
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, name := range []string{"observe", "metabase"} {
		wait.Add(1)
		go func(name string) {
			defer wait.Done()
			client := exec.CommandContext(ctx, python, "-u", file, "client", root, name)
			client.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
			data, err := client.Output()
			if err == nil && !strings.Contains(string(data), `"server": "`+name+`"`) {
				err = io.ErrUnexpectedEOF
			}
			errors <- err
		}(name)
	}
	var received []struct {
		ID     int    `json:"id"`
		Server string `json:"server"`
	}
	for range 2 {
		if !scanner.Scan() {
			t.Fatal("relay disconnected")
		}
		var envelope struct {
			ID     int    `json:"id"`
			Server string `json:"server"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		received = append(received, envelope)
	}
	// Reply in reverse order; overlapping client request IDs must not collide.
	for i := len(received) - 1; i >= 0; i-- {
		r := received[i]
		if err = json.NewEncoder(input).Encode(map[string]any{"id": r.ID, "response": map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]string{"server": r.Server}}}); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
	for range 2 {
		if err = <-errors; err != nil {
			t.Fatal(err)
		}
	}
	_ = input.Close()
	if err = server.Wait(); err != nil {
		t.Fatal("relay did not stop after host disconnected", err)
	}
	if _, err = os.Stat(filepath.Join(root, "mcp.sock")); !os.IsNotExist(err) {
		t.Fatal("relay socket retained after shutdown")
	}
}

// Opt in to exercise the qualified host binary against a disposable HTTP MCP.
// No model turn, host login, or production MCP service is used by this test.
func TestMCPNativeIntegration(t *testing.T) {
	if os.Getenv("PADDOCK_MCP_NATIVE_TEST") != "1" {
		t.Skip("set PADDOCK_MCP_NATIVE_TEST=1 to test the installed Codex protocol")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CODEX_BIN", path)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			var params struct {
				Version string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(request.Params, &params)
			result = map[string]any{"protocolVersion": params.Version, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read_status", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "native MCP worked"}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		default:
			t.Errorf("unexpected upstream MCP method: %s", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	home := t.TempDir()
	marker := filepath.Join(home, "UNTRUSTED-EXECUTED")
	config := hostMCPConfig{Servers: map[string]map[string]any{
		"trusted":   {"url": server.URL + "/mcp"},
		"untrusted": {"command": "/bin/sh", "args": []string{"-c", "touch " + quote(marker)}},
	}}
	data, err := toml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "config.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	set := trustedMCPSet{home: home, binary: path, config: config, names: []string{"trusted"}, grants: map[string]MCPTrust{"trusted": {}}}
	t.Setenv("PADDOCK_CODEX_BIN", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	native, stop, err := startNativeMCP(ctx, set, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	broker := mcpBroker{native: native, thread: native.thread}
	inventory, err := broker.inventory(ctx, "trusted")
	if err != nil || len(inventory.Tools) != 1 || inventory.ToolsError != nil {
		t.Fatalf("native MCP discovery: %+v %v", inventory, err)
	}
	ready, unavailable := readyTrustedMCPs(ctx, broker, []string{"trusted"})
	if len(ready) != 1 || ready[0] != "trusted" || len(unavailable) != 0 {
		t.Fatalf("native MCP startup catalog: ready=%v unavailable=%v", ready, unavailable)
	}
	var response json.RawMessage
	if err = native.call(ctx, "mcpServer/tool/call", map[string]any{"threadId": native.thread, "server": "trusted", "tool": "read_status", "arguments": map[string]any{}}, &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "native MCP worked") {
		t.Fatalf("native tool call: %s", response)
	}
	stop()
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("native broker started an untrusted host command")
	}
}
