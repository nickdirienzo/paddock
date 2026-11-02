package paddock

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed agent_guide.md
var AgentGuide string

const skillMarker = "<!-- Managed by paddock skill setup; remove with paddock skill remove. -->"

func SkillStub(executable string) string {
	return "---\nname: paddock\ndescription: Use Paddock to launch, inspect, delegate, and reconnect local coding tasks, or handle watched pull requests and replies. Use when asked to coordinate work with Paddock or when PADDOCK_TASK_ID identifies the current task.\n---\n" + skillMarker + "\n\nRead the guide from the installed Paddock CLI and follow it for this task:\n\n```sh\n" + quote(executable) + " skill show\n```\n\nThe guide is bundled with the executable, so CLI upgrades update the instructions.\nIf that executable has moved, use `paddock skill show` and rerun `paddock skill setup`\nfrom the installed binary to update this reference.\n"
}

type SkillInstallation struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
	State string `json:"state"`
}

func SkillInstallations(home, agent string) ([]SkillInstallation, error) {
	if agent == "" {
		agent = "all"
	}
	var entries []SkillInstallation
	for _, target := range []struct{ agent, dir string }{{"codex", ".agents"}, {"claude", ".claude"}} {
		if agent != "all" && agent != target.agent {
			continue
		}
		entry := SkillInstallation{Agent: target.agent, Path: filepath.Join(home, target.dir, "skills", "paddock", "SKILL.md")}
		state, err := inspectSkill(entry.Path)
		if err != nil {
			return nil, err
		}
		entry.State = state
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("--agent must be codex, claude, or all")
	}
	return entries, nil
}

func inspectSkill(path string) (string, error) {
	// Do not follow a same-named skill symlink into someone else's installation.
	if info, err := os.Lstat(filepath.Dir(path)); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "conflict", nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "conflict", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(body), skillMarker) {
		return "conflict", nil
	}
	return "managed", nil
}

// Skill setup is per user, independent of task state and repository contents.
func ConfigureSkills(home, agent, executable string, remove bool) ([]SkillInstallation, error) {
	entries, err := SkillInstallations(home, agent)
	if err != nil {
		return nil, err
	}
	// Preflight all targets before changing either agent's files.
	for _, entry := range entries {
		if entry.State == "conflict" {
			return nil, fmt.Errorf("preserving unmanaged or symlinked skill at %s", entry.Path)
		}
	}
	for _, entry := range entries {
		if remove {
			if entry.State == "missing" {
				continue
			}
			if err = os.Remove(entry.Path); err != nil {
				return nil, err
			}
			// Remove only an empty directory; preserve any other user resources.
			_ = os.Remove(filepath.Dir(entry.Path))
			continue
		}
		if err = os.MkdirAll(filepath.Dir(entry.Path), 0755); err != nil {
			return nil, err
		}
		if err = writeSkill(entry.Path, SkillStub(executable)); err != nil {
			return nil, err
		}
	}
	return SkillInstallations(home, agent)
}

func writeSkill(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".paddock-skill-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.WriteString(content); err != nil {
		return err
	}
	if err = f.Chmod(0644); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = replaceFile(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
