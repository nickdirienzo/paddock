//go:build !windows

package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in with a stopped, retained task. This exercises the real
// gateway without starting its coding agent or changing its conversation.
func TestLiveClipboardUpload(t *testing.T) {
	id := os.Getenv("PADDOCK_LIVE_CLIPBOARD_TASK")
	if id == "" {
		t.Skip("set PADDOCK_LIVE_CLIPBOARD_TASK to a stopped OpenShell task")
	}
	home, err := StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.RuntimeName() != "openshell" || task.OpenShell == nil {
		t.Fatal("live task is not an OpenShell task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, err := inspectTaskSandbox(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "Stopped" {
		t.Fatal("live test requires stopped compute; refusing to disturb the active task")
	}
	if _, err = task.OpenShell.Config.call(ctx, "sandbox", "start", task.OpenShell.Name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := task.OpenShell.Config.call(cleanup, "sandbox", "stop", task.OpenShell.Name); err != nil {
			t.Error("could not return maintenance compute to stopped state", err)
		}
	}()
	source, err := os.CreateTemp(os.TempDir(), "codex-clipboard-*.png")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(source.Name())
	data, _ := base64.StdEncoding.DecodeString(clipboardPNG)
	if _, err = source.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	destination, err := uploadClipboardImage(ctx, task, source.Name())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	actual, err := sandboxOutput(ctx, task, "/usr/bin/python3", "-c", "import hashlib,pathlib,sys;print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())", destination)
	if err != nil || actual != hex.EncodeToString(hash[:]) {
		t.Fatal("sandbox image bytes differ", err)
	}
	if _, err = sandboxOutput(ctx, task, "/bin/rm", "--", destination); err != nil {
		t.Fatal(err)
	}
	exerciseLiveClipboardTerminal(t, task.ID, source.Name(), hex.EncodeToString(hash[:]))
	if os.Getenv("PADDOCK_LIVE_CLAUDE_SMOKE") == "1" {
		// An explicitly enabled, tool-free inference confirms provider injection.
		task.Worktree = "/tmp"
		response, err := sandboxOutput(ctx, task, "/usr/local/bin/claude", "-p", "Reply with exactly PADDOCK_AUTH_OK.", "--tools", "", "--disable-slash-commands", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence")
		if err != nil || strings.TrimSpace(response) != "PADDOCK_AUTH_OK" {
			t.Fatal("Claude auth smoke test failed; native response withheld", err)
		}
		t.Log("Claude completed a tool-free model response using the renewed gateway credential")
	}
}
