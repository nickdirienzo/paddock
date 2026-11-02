package paddock

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// ReuseMiseTrust handles sibling worktrees of bare repositories, for which mise
// cannot inherit trust from a main checkout. It never edits mise's trust records
// directly, changes paranoid mode, or trusts the whole Paddock worktree directory.
func (s Service) ReuseMiseTrust(ctx context.Context, task Task) (bool, error) {
	trusted, err := RepoTrusted(s.Store.Home, task.CommonDir)
	if err != nil || !trusted {
		return false, err
	}
	mise, err := executable("PADDOCK_MISE_BIN", "mise")
	if err != nil {
		return false, nil
	} // mise is optional
	// Custom discovery rules are not covered by the conservative snapshot below.
	for _, key := range []string{"MISE_CONFIG_FILE", "MISE_OVERRIDE_CONFIG_FILENAMES", "MISE_DEFAULT_CONFIG_FILENAME"} {
		if os.Getenv(key) != "" {
			return false, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	setting := func(name string) (string, error) {
		b, err := miseOutput(ctx, mise, home, "settings", "get", name)
		return strings.TrimSpace(string(b)), err
	}
	paranoid, err := setting("paranoid")
	if err != nil {
		return false, err
	}
	if paranoid != "false" {
		return false, nil
	}
	ignored, err := setting("ignored_config_paths")
	if err != nil {
		return false, err
	}
	if ignored != "[]" {
		return false, nil
	} // preserve explicit ignore policy
	target, err := filepath.EvalSymlinks(task.Worktree)
	if err != nil {
		return false, err
	}
	blocked, err := miseIgnored(target, home)
	if err != nil || blocked {
		return false, err
	}
	status, err := miseRootStatus(ctx, mise, target, home)
	if err != nil {
		return false, err
	}
	if status != "untrusted" {
		return false, nil
	}
	want, err := miseConfigSnapshot(target)
	if err != nil {
		return false, err
	}
	if len(want) == 0 {
		return false, nil
	}
	configFile := ""
	for _, name := range []string{"mise.toml", ".mise.toml", ".mise/config.toml", "mise/config.toml", ".config/mise.toml", ".config/mise/config.toml"} {
		if _, exists := want[filepath.FromSlash(name)]; exists {
			configFile = filepath.Join(target, filepath.FromSlash(name))
			break
		}
	}
	if configFile == "" {
		return false, nil
	}
	repo, err := discoverRepository(ctx, target)
	if err != nil {
		return false, err
	}
	if repo.CommonDir != task.CommonDir {
		return false, fmt.Errorf("task repository membership changed")
	}
	listing, err := git(ctx, target, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	for _, field := range strings.Split(listing, "\x00") {
		root, ok := strings.CutPrefix(field, "worktree ")
		if !ok {
			continue
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil || root == target {
			continue
		}
		got, err := miseConfigSnapshot(root)
		if err != nil || !reflect.DeepEqual(got, want) {
			continue
		}
		// Stale worktree registrations must not import trust from a reused path.
		sourceRepo, err := discoverRepository(ctx, root)
		if err != nil || sourceRepo.CommonDir != task.CommonDir {
			continue
		}
		status, err = miseRootStatus(ctx, mise, root, home)
		if err != nil {
			return false, err
		}
		if status != "trusted" {
			continue
		}
		// Recheck after external discovery before granting the native decision.
		current, err := miseConfigSnapshot(target)
		if err != nil || !reflect.DeepEqual(current, want) {
			return false, fmt.Errorf("mise configs changed during trust comparison")
		}
		current, err = miseConfigSnapshot(root)
		if err != nil || !reflect.DeepEqual(current, want) {
			return false, fmt.Errorf("source mise configs changed during trust comparison")
		}
		blocked, err = miseIgnored(target, home)
		if err != nil || blocked {
			return false, err
		}
		if _, err = miseOutput(ctx, mise, target, "trust", configFile); err != nil {
			return false, err
		}
		status, err = miseRootStatus(ctx, mise, target, home)
		if err != nil {
			return false, err
		}
		if status != "trusted" {
			return false, fmt.Errorf("mise did not confirm inherited trust")
		}
		return true, nil
	}
	return false, nil
}

func miseOutput(ctx context.Context, mise, cwd string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, mise, append([]string{"-C", cwd}, args...)...)
	c.Env = append(credentialFreeEnvironment(os.Environ()), "MISE_YES=0", "MISE_COLOR=0", "NO_COLOR=1")
	b, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("mise %s failed: %w", args[0], err)
	}
	return b, nil
}

func miseRootStatus(ctx context.Context, mise, root, home string) (string, error) {
	b, err := miseOutput(ctx, mise, root, "trust", "--show")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, status := range []string{"trusted", "untrusted"} {
			path, ok := strings.CutSuffix(line, ": "+status)
			if !ok {
				continue
			}
			if strings.HasPrefix(path, "~/") {
				path = filepath.Join(home, path[2:])
			}
			canonical, err := filepath.EvalSymlinks(path)
			if err == nil && canonical == root {
				return status, nil
			}
		}
	}
	// Unknown output, file-scoped trust, and ignored configs do not grant trust.
	return "", nil
}

func miseIgnored(target, home string) (bool, error) {
	state := os.Getenv("MISE_STATE_DIR")
	if state == "" {
		state = os.Getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		state = filepath.Join(state, "mise")
	}
	entries, err := os.ReadDir(filepath.Join(state, "ignored-configs"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		path := filepath.Join(state, "ignored-configs", entry.Name())
		ignored, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if entry.Type()&os.ModeSymlink == 0 {
			if strings.HasSuffix(entry.Name(), ".hash") || strings.HasSuffix(entry.Name(), ".monorepo") {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return false, err
			}
			ignored = strings.TrimSpace(string(b))
			if !filepath.IsAbs(ignored) {
				continue
			}
		}
		rel, err := filepath.Rel(ignored, target)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return true, nil
		}
	}
	return false, nil
}

// Compare all root mise variants, including inactive environments, idiomatic
// version files, and config directories. A symlink cannot import outside content.
func miseConfigSnapshot(root string) (map[string][32]byte, error) {
	result := map[string][32]byte{}
	var total int64
	add := func(path string, info fs.FileInfo) error {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mise config is not a regular file")
		}
		total += info.Size()
		if total > 8<<20 || len(result) >= 256 {
			return fmt.Errorf("mise config snapshot exceeds limit")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[rel] = sha256.Sum256(b)
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		config := miseTOMLName(name) || name == ".tool-versions" || name == "package.json" || name == ".nvmrc" || strings.HasSuffix(name, "-version") || strings.HasSuffix(name, "-versions")
		if !config {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if err = add(filepath.Join(root, name), info); err != nil {
			return nil, err
		}
	}
	configDir := filepath.Join(root, ".config")
	if info, err := os.Lstat(configDir); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("mise config directory is not a regular directory")
		}
		entries, err := os.ReadDir(configDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !miseTOMLName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if err = add(filepath.Join(configDir, entry.Name()), info); err != nil {
				return nil, err
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, dir := range []string{filepath.Join(root, ".mise"), filepath.Join(root, "mise"), filepath.Join(root, ".config", "mise")} {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err = filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			return add(path, info)
		}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func miseTOMLName(name string) bool {
	name = strings.TrimPrefix(name, ".")
	return (strings.HasPrefix(name, "mise.") || strings.HasPrefix(name, "rtx.")) && strings.HasSuffix(name, ".toml")
}
