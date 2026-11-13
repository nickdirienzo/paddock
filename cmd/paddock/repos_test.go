package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMCPTrustCLI(t *testing.T) {
	home, codex, repo := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PADDOCK_HOME", home)
	t.Setenv("CODEX_HOME", codex)
	if b, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte("[mcp_servers.observe]\nurl=\"https://observe.example/mcp\"\n[mcp_servers.metabase]\nurl=\"https://metabase.example/mcp\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runRepo([]string{"add", "project", repo}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRepo([]string{"trust", "project", "--mcp", "observe, metabase"}, &out); err != nil || !strings.Contains(out.String(), "execute on the host") {
		t.Fatalf("host MCP grant failed or hid its execution boundary: %s %v", out.String(), err)
	}
	read := func() map[string]map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(home, "repos.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Grants map[string]map[string]any `json:"trusted_mcps"`
		}
		if err = json.Unmarshal(b, &record); err != nil {
			t.Fatal(err)
		}
		return record.Grants
	}
	if len(read()) != 1 {
		t.Fatal("MCP grants not persisted")
	}
	if err := runRepo([]string{"untrust", "project", "--mcp", "observe"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, grants := range read() {
		if len(grants) != 1 || grants["metabase"] == nil {
			t.Fatal("wrong MCP revoked")
		}
	}
	for _, args := range [][]string{{"list", "--mcp", "observe"}, {"trust", "project", "--mcp", "observe,"}, {"trust", "project", "--mcp", "missing"}} {
		if err := runRepo(args, io.Discard); err == nil {
			t.Fatal("accepted invalid arguments", args)
		}
	}
	if err := runRepo([]string{"untrust", "project"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 0 {
		t.Fatal("project untrust retained MCP grants")
	}
}
