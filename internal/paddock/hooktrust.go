package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type codexHookState struct {
	TrustedHash string `toml:"trusted_hash"`
	Enabled     *bool  `toml:"enabled"`
}

var hookSuffix = regexp.MustCompile(`^:[a-z_]+:[0-9]+:[0-9]+$`)
var hookHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Reuse only decisions Codex already saved, from registered worktrees of the
// same repository with identical hook-source files. Codex still validates the
// trusted hash against the current definition. Never manufacture a trust hash.
func codexHookTrust(ctx context.Context, task Task) (string, error) {
	if task.Session.Agent != "codex" || !task.Session.TrustProject {
		return "", nil
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Hooks struct {
			State map[string]codexHookState `toml:"state"`
		} `toml:"hooks"`
	}
	if err := toml.Unmarshal(b, &config); err != nil {
		// Do not echo config contents, which may contain credentials.
		return "", fmt.Errorf("cannot parse Codex hook state in config.toml")
	}
	if len(config.Hooks.State) == 0 {
		return "", nil
	}
	target, err := filepath.EvalSymlinks(task.Worktree)
	if err != nil {
		return "", err
	}
	listing, err := git(ctx, task.Worktree, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	var roots []string
	for _, field := range strings.Split(listing, "\x00") {
		if root, ok := strings.CutPrefix(field, "worktree "); ok {
			// Git can retain prunable registrations after a path is removed or
			// reused. Check actual membership only for paths with saved state.
			for key := range config.Hooks.State {
				if strings.HasPrefix(key, filepath.Join(root, ".codex")+string(filepath.Separator)) {
					repo, err := discoverRepository(ctx, root)
					if err == nil && repo.CommonDir == task.CommonDir {
						roots = append(roots, root)
					}
					break
				}
			}
		}
	}
	states := inheritedHookStates(target, roots, config.Hooks.State)
	if len(states) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var entries []string
	for _, key := range keys {
		state := states[key]
		var fields []string
		if state.TrustedHash != "" {
			fields = append(fields, "trusted_hash="+tomlString(state.TrustedHash))
		}
		if state.Enabled != nil {
			fields = append(fields, fmt.Sprintf("enabled=%t", *state.Enabled))
		}
		entries = append(entries, tomlString(key)+"={"+strings.Join(fields, ",")+"}")
	}
	return "hooks.state={" + strings.Join(entries, ",") + "}", nil
}

func tomlString(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func inheritedHookStates(target string, roots []string, saved map[string]codexHookState) map[string]codexHookState {
	result := map[string]codexHookState{}
	conflicts := map[string]bool{}
	// Global and plugin hooks already have stable paths and remain Codex-owned.
	for _, name := range []string{"hooks.json", "config.toml"} {
		targetSource := filepath.Join(target, ".codex", name)
		targetBytes, err := os.ReadFile(targetSource)
		if err != nil {
			continue
		}
		for _, root := range roots {
			canonical, err := filepath.EvalSymlinks(root)
			if err != nil || canonical == target {
				continue
			}
			source := filepath.Join(root, ".codex", name)
			b, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(b, targetBytes) {
				continue
			}
			for key, state := range saved {
				suffix, ok := strings.CutPrefix(key, source)
				if !ok || !hookSuffix.MatchString(suffix) {
					continue
				}
				if state.TrustedHash != "" && !hookHash.MatchString(state.TrustedHash) {
					continue
				}
				destination := targetSource + suffix
				// A decision made in this exact worktree always wins, even a
				// cleared trust entry or an explicit disabled state.
				if _, exists := saved[destination]; exists {
					continue
				}
				if previous, exists := result[destination]; exists && !sameHookState(previous, state) {
					conflicts[destination] = true
				}
				result[destination] = state
			}
		}
	}
	// Conflicting decisions require review; map iteration cannot choose trust.
	for key := range conflicts {
		delete(result, key)
	}
	for key, state := range result {
		if state.TrustedHash == "" && (state.Enabled == nil || *state.Enabled) {
			delete(result, key)
		}
	}
	return result
}

func sameHookState(a, b codexHookState) bool {
	// Missing enabled means enabled in Codex.
	return a.TrustedHash == b.TrustedHash && (a.Enabled == nil || *a.Enabled) == (b.Enabled == nil || *b.Enabled)
}
