package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

var aliasName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type repoAliases struct {
	Version             int                            `json:"version"`
	Repositories        map[string]string              `json:"repositories"`
	TrustedRepositories []string                       `json:"trusted_repositories,omitempty"`
	TrustedMCPs         map[string]map[string]MCPTrust `json:"trusted_mcps,omitempty"`
}

// Readers see complete snapshots because writers replace the file atomically.
func ReadRepoAliases(home string) (map[string]string, error) {
	data, err := readRepoConfig(home)
	return data.Repositories, err
}

func readRepoConfig(home string) (repoAliases, error) {
	var data repoAliases
	b, err := os.ReadFile(filepath.Join(home, "repos.json"))
	if errors.Is(err, os.ErrNotExist) {
		return repoAliases{Version: 1, Repositories: map[string]string{}}, nil
	}
	if err != nil {
		return data, err
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return data, fmt.Errorf("read repo aliases: %w", err)
	}
	if data.Version != 1 || data.Repositories == nil {
		return data, fmt.Errorf("invalid or unsupported repos.json format")
	}
	for name, path := range data.Repositories {
		if !aliasName.MatchString(name) || !filepath.IsAbs(path) {
			return data, fmt.Errorf("invalid repo alias %q in repos.json", name)
		}
	}
	for _, path := range data.TrustedRepositories {
		if !filepath.IsAbs(path) {
			return data, fmt.Errorf("invalid trusted repository path %q", path)
		}
	}
	return data, nil
}

func RepoTrusted(home, commonDir string) (bool, error) {
	data, err := readRepoConfig(home)
	if err != nil {
		return false, err
	}
	return slices.Contains(data.TrustedRepositories, commonDir), nil
}

func ResolveRepoArgument(home, value string) (string, error) {
	// Absolute paths and explicit relative paths such as ./mirage bypass aliases.
	if !aliasName.MatchString(value) {
		return value, nil
	}
	aliases, err := ReadRepoAliases(home)
	if err != nil {
		return "", err
	}
	if path, ok := aliases[value]; ok {
		return path, nil
	}
	return value, nil
}

func (s *Store) AddRepo(ctx context.Context, name, path string) (string, error) {
	if !aliasName.MatchString(name) {
		return "", fmt.Errorf("invalid repo name %q; use letters, numbers, underscores, or hyphens, starting with a letter or number", name)
	}
	repo, err := discoverRepository(ctx, path)
	if err != nil {
		return "", err
	}
	err = s.withLock(func() error {
		data, err := readRepoConfig(s.Home)
		if err != nil {
			return err
		}
		if old, ok := data.Repositories[name]; ok && old != repo.Root {
			return fmt.Errorf("repo %q already maps to %s; remove the alias before replacing it", name, old)
		}
		data.Repositories[name] = repo.Root
		return writeJSONAtomic(filepath.Join(s.Home, "repos.json"), data)
	})
	return repo.Root, err
}

func (s *Store) RemoveRepo(name string) error {
	return s.withLock(func() error {
		data, err := readRepoConfig(s.Home)
		if err != nil {
			return err
		}
		if _, ok := data.Repositories[name]; !ok {
			return fmt.Errorf("repo alias %q not found", name)
		}
		delete(data.Repositories, name)
		return writeJSONAtomic(filepath.Join(s.Home, "repos.json"), data)
	})
}

// Trust belongs to the canonical Git common directory, so linked worktrees and
// delegated children share the explicit decision. Aliases are only shortcuts.
func (s *Store) SetRepoTrust(ctx context.Context, nameOrPath string, trusted bool) (string, error) {
	path, err := ResolveRepoArgument(s.Home, nameOrPath)
	if err != nil {
		return "", err
	}
	repo, err := discoverRepository(ctx, path)
	if err != nil {
		return "", err
	}
	err = s.withLock(func() error {
		data, err := readRepoConfig(s.Home)
		if err != nil {
			return err
		}
		data.TrustedRepositories = slices.DeleteFunc(data.TrustedRepositories, func(path string) bool { return path == repo.CommonDir })
		if trusted {
			data.TrustedRepositories = append(data.TrustedRepositories, repo.CommonDir)
		} else {
			delete(data.TrustedMCPs, repo.CommonDir)
		}
		return writeJSONAtomic(filepath.Join(s.Home, "repos.json"), data)
	})
	return repo.CommonDir, err
}
