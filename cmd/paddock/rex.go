package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed rex_launcher.lua
var rexLauncher string

const rexConfigBegin = "-- BEGIN paddock task launcher\n"
const rexConfigEnd = "-- END paddock task launcher\n"
const rexDefaultKey = "alt+cmd+n"

const rexHelp = `Usage: paddock rex setup [--config path] [--key shortcut]
       paddock rex remove [--config path]

Install a New Paddock Task command-palette action and keyboard shortcut.
Default shortcut: Option-Command-N. The form opens in a floating Rex pane.
Setup updates only the marked Paddock section of init.lua; remove deletes it.
Configuration: --config, then REX_CONFIG, then $XDG_CONFIG_HOME/rex/init.lua
or ~/.config/rex/init.lua. Run rex config reload after setup or removal.
`

func rexConfigPath(override string) (string, error) {
	if override == "" {
		override = os.Getenv("REX_CONFIG")
	}
	if override != "" {
		return filepath.Abs(expandRepositoryPath(override))
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Abs(filepath.Join(dir, "rex", "init.lua"))
}

// A Lua long string preserves paths and keys literally, including quotes,
// backslashes, and non-ASCII characters, without involving a shell.
func luaString(value string) string {
	equals := ""
	for strings.Contains(value, "]"+equals+"]") {
		equals += "="
	}
	return "[" + equals + "[" + value + "]" + equals + "]"
}

// The Rex server can have a minimal GUI PATH. Capture the setup shell's tool
// locations and explicit non-secret overrides, then execute without another
// shell startup replacing them. Never copy the whole shell environment.
func rexLauncherCommand(executable string) []string {
	command := []string{"/usr/bin/env", "PATH=" + filepath.Dir(executable) + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, key := range []string{"PADDOCK_HOME", "PADDOCK_CODEX_BIN", "PADDOCK_CLAUDE_BIN", "PADDOCK_REX_BIN", "PADDOCK_SCREEN_BIN", "PADDOCK_MISE_BIN", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "MISE_STATE_DIR", "MISE_CONFIG_DIR", "XDG_STATE_HOME"} {
		if value := os.Getenv(key); value != "" {
			command = append(command, key+"="+value)
		}
	}
	return append(command, executable, "new", "--terminal", "rex")
}

func luaStrings(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, luaString(value))
	}
	return "{ " + strings.Join(quoted, ", ") + " }"
}

func configureRexLauncher(path, executable, shortcut string, remove bool) error {
	mode := os.FileMode(0644)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("preserving non-regular or symlinked Rex configuration at %s", path)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) && remove {
		return nil
	}
	content := string(body)
	begin, end := strings.Index(content, rexConfigBegin), strings.Index(content, rexConfigEnd)
	if strings.Count(content, rexConfigBegin) > 1 || strings.Count(content, rexConfigEnd) > 1 ||
		(begin == -1) != (end == -1) || (begin >= 0 && end < begin) ||
		(begin > 0 && content[begin-1] != '\n') || (end > 0 && content[end-1] != '\n') {
		return fmt.Errorf("malformed Paddock markers in %s; preserving the configuration", path)
	}
	block := ""
	if !remove {
		block = rexConfigBegin + strings.NewReplacer(
			"@LAUNCHER_COMMAND@", luaStrings(rexLauncherCommand(executable)), "@SHORTCUT@", luaString(shortcut),
		).Replace(rexLauncher) + rexConfigEnd
	}
	if begin >= 0 {
		content = content[:begin] + block + content[end+len(rexConfigEnd):]
	} else if !remove {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += block
	}
	if content == string(body) {
		return nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".paddock-rex-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.WriteString(content); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func runRex(args []string, out io.Writer) error {
	flags, pos, err := parse(args, "config key")
	if err != nil {
		return err
	}
	if flags["help"] != "" || len(pos) == 0 {
		fmt.Fprint(out, rexHelp)
		return nil
	}
	if len(pos) != 1 || (pos[0] != "setup" && pos[0] != "remove") {
		return fmt.Errorf("use paddock rex setup or remove; see paddock rex --help")
	}
	remove := pos[0] == "remove"
	if remove && flags["key"] != "" {
		return fmt.Errorf("--key is only supported by rex setup")
	}
	path, err := rexConfigPath(flags["config"])
	if err != nil {
		return err
	}
	shortcut := flags["key"]
	if shortcut == "" {
		shortcut = rexDefaultKey
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	if err = configureRexLauncher(path, executable, shortcut, remove); err != nil {
		return err
	}
	if remove {
		fmt.Fprintln(out, "Removed Paddock launcher from", path)
	} else {
		fmt.Fprintf(out, "Installed New Paddock Task in %s\nShortcut: %s\n", path, shortcut)
	}
	fmt.Fprintln(out, "Run rex config reload to apply the change.")
	return nil
}
