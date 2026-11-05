package paddock

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const clipboardPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJ1sAAAAASUVORK5CYII="

func TestClipboardPasteAcrossEveryReadBoundary(t *testing.T) {
	path := "/tmp/user/rex-clipboard-B9342264-6231-430E-84FF-71C94DA1B05B.png"
	second := "/tmp/user/codex-clipboard-fR2JA0.png"
	input := "typed /src/file\x1b[200~look at " + path + " and " + second + "\x1b[201~\r\x03"
	want := strings.ReplaceAll(strings.ReplaceAll(input, path, "/sandbox/one.png"), second, "/sandbox/two.png")
	for split := 0; split <= len(input); split++ {
		var output bytes.Buffer
		var paths []string
		filter := newClipboardInput(&output, []string{"/tmp/user"}, func(source string) (string, error) {
			paths = append(paths, source)
			if source == path {
				return "/sandbox/one.png", nil
			}
			return "/sandbox/two.png", nil
		})
		for _, chunk := range []string{input[:split], input[split:]} {
			if _, err := filter.Write([]byte(chunk)); err != nil {
				t.Fatal(err)
			}
		}
		if err := filter.Flush(); err != nil {
			t.Fatal(err)
		}
		if output.String() != want || len(paths) != 2 || paths[0] != path || paths[1] != second {
			t.Fatalf("split %d: %q %v", split, output.String(), paths)
		}
	}
	var output bytes.Buffer
	filter := newClipboardInput(&output, []string{"/tmp/user"}, func(string) (string, error) { return "/sandbox/image.png", nil })
	for _, char := range []byte(path) {
		if _, err := filter.Write([]byte{char}); err != nil {
			t.Fatal(err)
		}
	}
	if output.String() != "/sandbox/image.png" {
		t.Fatalf("bytewise paste: %q", output.String())
	}
}

func TestClipboardPasteLeavesOtherInputAndReportsTransferFailure(t *testing.T) {
	for _, input := range []string{"ordinary typing\x1b[A\x1b[B\r\x03", "/tmp/user/other.png", "/tmp/other/rex-clipboard-B9342264-6231-430E-84FF-71C94DA1B05B.png", "/tmp/user/rex-clipboard-not-a-uuid.png", "/tmp/user/rex-clipboard-", strings.Repeat("/", 4096)} {
		var output bytes.Buffer
		filter := newClipboardInput(&output, []string{"/tmp/user"}, func(string) (string, error) { t.Fatal("unselected file transferred"); return "", nil })
		_, _ = filter.Write([]byte(input))
		_ = filter.Flush()
		if output.String() != input {
			t.Fatalf("changed ordinary input: %q", output.String())
		}
	}
	var output bytes.Buffer
	filter := newClipboardInput(&output, []string{"/tmp/user"}, func(string) (string, error) { return "", errors.New("sandbox upload failed") })
	_, _ = filter.Write([]byte("\x1b[200~/tmp/user/codex-clipboard-abc123.png\x1b[201~"))
	_ = filter.Flush()
	if !strings.Contains(output.String(), "Paddock could not transfer") || !strings.HasSuffix(output.String(), "\x1b[201~") {
		t.Fatal("transfer failure was hidden or damaged bracketed paste")
	}
}

func TestClipboardSnapshotOnlyCopiesSelectedRegularImages(t *testing.T) {
	dir := t.TempDir()
	data, _ := base64.StdEncoding.DecodeString(clipboardPNG)
	source := filepath.Join(dir, "codex-clipboard-abc123.png")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	staged, destination, cleanup, err := snapshotClipboardImage(source, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(actual, data) || !strings.HasPrefix(destination, "/sandbox/.paddock/attachments/") {
		t.Fatal("image was not snapshotted exactly", err)
	}
	if err = os.WriteFile(source, []byte("changed after selection"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, _ = os.ReadFile(staged)
	if !bytes.Equal(actual, data) {
		t.Fatal("snapshot changed with source")
	}
	cleanup()
	if _, err = os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("snapshot retained after transfer")
	}
	if _, _, _, err = snapshotClipboardImage(source, []string{dir}); err == nil {
		t.Fatal("transferred non-image file disguised as PNG")
	}
	if _, _, _, err = snapshotClipboardImage(source, []string{t.TempDir()}); err == nil {
		t.Fatal("transferred from another directory")
	}
	link := filepath.Join(dir, "codex-clipboard-symlink.png")
	if err = os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = snapshotClipboardImage(link, []string{dir}); err == nil {
		t.Fatal("followed clipboard symlink")
	}
	large := filepath.Join(dir, "codex-clipboard-large.png")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(maxClipboardImage + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, _, _, err = snapshotClipboardImage(large, []string{dir}); err == nil {
		t.Fatal("accepted oversized image")
	}
}

func TestPasteHelperPreservesRecoveryNoticeAndRejectsStaleLaunch(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Runtime, task.Status, task.Worktree = "openshell", "running", "/sandbox/worktree"
	task.OpenShell = &OpenShellTask{Name: "fixture", Config: OpenShellConfig{Binary: "/bin/echo"}}
	task.Session.Agent, task.Session.AgentPath = "codex", "/usr/local/bin/codex"
	task.Session.Resume, task.Session.Generation = true, 3
	task.Session.ConversationID = "saved-conversation"
	task.Session.ResumeNotice = ""
	task.Prompt = "Original assignment must not be replayed"
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	notice := "Paddock restored this worktree from Git; uncommitted files were not recovered."
	_, command, err := pasteAgentCommand(store, task.ID, 3, notice)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(command.Args, " ")
	if !strings.Contains(args, "resume saved-conversation -- "+notice) || strings.Contains(args, task.Prompt) {
		t.Fatal("paste helper lost resume identity/notice or replayed the original assignment")
	}
	if _, _, err = pasteAgentCommand(store, task.ID, 2, notice); err == nil {
		t.Fatal("stale helper was allowed to launch")
	}
	if err = store.update(task.ID, func(t *Task) error { t.Status = "stopped"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err = pasteAgentCommand(store, task.ID, 3, notice); err == nil {
		t.Fatal("helper launched outside its running supervisor")
	}
}
