package paddock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpTrustFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	s := openTestStore(t, t.TempDir())
	repo := repoFixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	config := `[mcp_servers.observe]
url = "https://observe.example/mcp"
http_headers = { Authorization = "Bearer host-only-secret" }
disabled_tools = ["disabled_tool"]
[mcp_servers.metabase]
url = "https://metabase.tailnet.ts.net/api/metabase-mcp"
[mcp_servers.local]
command = "/usr/bin/false"
[mcp_servers.disabled]
url = "https://disabled.example/mcp"
enabled = false
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRepo(context.Background(), "project", repo); err != nil {
		t.Fatal(err)
	}
	return s, repo, home
}

func TestMCPTrustScopedPinnedAndRevocable(t *testing.T) {
	s, repo, host := mcpTrustFixture(t)
	ctx := context.Background()
	common, err := s.SetRepoMCPTrust(ctx, "project", []string{"observe"}, true)
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadTrustedMCPs(s.Home, common)
	if err != nil || len(set.names) != 1 || set.names[0] != "observe" {
		t.Fatalf("%+v %v", set, err)
	}
	if err = set.authorize(s.Home, common, "metabase"); err == nil {
		t.Fatal("unselected server authorized")
	}
	if err = set.authorize(s.Home, common, "observe"); err != nil {
		t.Fatal(err)
	}
	if other, err := loadTrustedMCPs(s.Home, "another-repository"); err != nil || len(other.names) != 0 {
		t.Fatal("trust leaked across repositories")
	}
	linked := filepath.Join(t.TempDir(), "linked")
	gitTest(t, repo, "worktree", "add", "-b", "linked", linked)
	if _, err = s.SetRepoMCPTrust(ctx, linked, []string{"metabase"}, true); err != nil {
		t.Fatal(err)
	}
	set, err = loadTrustedMCPs(s.Home, common)
	if err != nil || len(set.names) != 2 {
		t.Fatalf("linked worktree lost grants: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(s.Home, "repos.json"))
	if err != nil || strings.Contains(string(saved), "host-only-secret") {
		t.Fatal("trust record contains credentials", err)
	}
	if _, err = s.SetRepoMCPTrust(ctx, "project", []string{"observe"}, false); err != nil {
		t.Fatal(err)
	}
	if err = set.authorize(s.Home, common, "observe"); err == nil {
		t.Fatal("revocation did not affect an active broker")
	}
	if err = set.authorize(s.Home, common, "metabase"); err != nil {
		t.Fatal("revocation affected another grant", err)
	}
	if trusted, _ := RepoTrusted(s.Home, common); !trusted {
		t.Fatal("individual MCP revocation removed project trust")
	}
	configPath := filepath.Join(host, "config.toml")
	config, _ := os.ReadFile(configPath)
	config = []byte(strings.ReplaceAll(string(config), "metabase.tailnet.ts.net", "replacement.example"))
	if err = os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadTrustedMCPs(s.Home, common); err == nil {
		t.Fatal("changed endpoint retained trust")
	}
	if _, err = s.SetRepoMCPTrust(ctx, "project", []string{"metabase"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRepoTrust(ctx, "project", false); err != nil {
		t.Fatal(err)
	}
	if set, err = loadTrustedMCPs(s.Home, common); err != nil || len(set.names) != 0 {
		t.Fatal("project untrust retained MCP access", err)
	}
	if _, err = s.SetRepoTrust(ctx, "project", true); err != nil {
		t.Fatal(err)
	}
	if set, err = loadTrustedMCPs(s.Home, common); err != nil || len(set.names) != 0 {
		t.Fatal("project trust silently restored revoked MCP grants", err)
	}
}

func TestMCPTrustRejectsUnsafeOrUnknownServersAtomically(t *testing.T) {
	s, _, _ := mcpTrustFixture(t)
	for _, name := range []string{"missing", "local", "disabled", "bad.name", "../observe", ""} {
		if _, err := s.SetRepoMCPTrust(context.Background(), "project", []string{"observe", name}, true); err == nil {
			t.Fatalf("trusted %q", name)
		}
	}
	data, err := readRepoConfig(s.Home)
	if err != nil || len(data.TrustedMCPs) != 0 || len(data.TrustedRepositories) != 0 {
		t.Fatal("failed grant partially persisted", err)
	}
	for _, endpoint := range []string{"http://example.com/mcp", "https://user:secret@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/mcp#secret", "file:///tmp/mcp"} {
		if _, err := fingerprintMCP("/host", map[string]any{"url": endpoint}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("accepted unsafe endpoint or leaked credentials")
		}
	}
}

type fakeMCPNative struct{ calls []string }

func (f *fakeMCPNative) call(_ context.Context, method string, params any, result any) error {
	f.calls = append(f.calls, method)
	var value any
	switch method {
	case "mcpServerStatus/list":
		value = map[string]any{"data": []any{map[string]any{"name": "observe", "tools": map[string]any{
			"search":        map[string]any{"name": "search", "inputSchema": map[string]any{"type": "object"}},
			"disabled_tool": map[string]any{"name": "disabled_tool", "inputSchema": map[string]any{"type": "object"}},
		}, "resources": []any{}, "resourceTemplates": []any{}}}}
	case "mcpServer/tool/call":
		p := params.(map[string]any)
		if p["server"] != "observe" || p["threadId"] != "host-thread" || p["tool"] != "search" {
			panic("incorrect native destination")
		}
		value = map[string]any{"content": []any{map[string]string{"type": "text", "text": "found"}}}
	case "mcpServer/resource/read":
		value = map[string]any{"contents": []any{map[string]string{"uri": "resource://one", "text": "value"}}}
	default:
		panic("unexpected native method: " + method)
	}
	b, _ := json.Marshal(value)
	return json.Unmarshal(b, result)
}

func TestMCPBrokerOnlyForwardsTrustedMCPMethodsAndCatalogTools(t *testing.T) {
	s, _, _ := mcpTrustFixture(t)
	common, err := s.SetRepoMCPTrust(context.Background(), "project", []string{"observe"}, true)
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadTrustedMCPs(s.Home, common)
	if err != nil {
		t.Fatal(err)
	}
	native := &fakeMCPNative{}
	broker := mcpBroker{storeHome: s.Home, commonDir: common, set: set, native: native, thread: "host-thread"}
	for _, tc := range []struct {
		server, method, params string
		wantError              bool
	}{
		{"observe", "initialize", `{"protocolVersion":"2025-06-18"}`, false},
		{"observe", "tools/list", `{}`, false},
		{"observe", "tools/call", `{"name":"search","arguments":{"q":"query"}}`, false},
		{"observe", "resources/read", `{"uri":"resource://one"}`, false},
		{"observe", "tools/call", `{"name":"disabled_tool"}`, true},
		{"metabase", "tools/call", `{"name":"search"}`, true},
		{"observe", "turn/start", `{}`, true},
		{"observe", "process/spawn", `{}`, true},
		{"observe", "mcpServer/tool/call", `{"server":"metabase","tool":"search"}`, true},
		{"observe", "tools/call", `{invalid`, true},
	} {
		response := broker.dispatch(context.Background(), tc.server, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`"client-id"`), Method: tc.method, Params: json.RawMessage(tc.params)})
		encoded, _ := json.Marshal(response)
		var reply struct {
			ID    string `json:"id"`
			Error any    `json:"error"`
		}
		if json.Unmarshal(encoded, &reply) != nil || reply.ID != "client-id" || (reply.Error != nil) != tc.wantError {
			t.Fatalf("%s %s: %s", tc.server, tc.method, encoded)
		}
		if tc.method == "tools/list" && strings.Contains(string(encoded), "disabled_tool") {
			t.Fatal("disabled host tool leaked into sandbox catalog")
		}
	}
	before := len(native.calls)
	if _, err = s.SetRepoTrust(context.Background(), "project", false); err != nil {
		t.Fatal(err)
	}
	response := broker.dispatch(context.Background(), "observe", mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"search"}`)})
	encoded, _ := json.Marshal(response)
	if !strings.Contains(string(encoded), `"error"`) || len(native.calls) != before {
		t.Fatal("revoked broker reached upstream")
	}
}
