package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// MCPTrust grants this repository access to one host MCP connection. Only
// non-secret references and a fingerprint are persisted, never OAuth material.
type MCPTrust struct {
	URL        string `json:"url"`
	CodexHome  string `json:"codex_home"`
	ConfigHash string `json:"config_hash"`
}

type hostMCPConfig struct {
	Servers map[string]map[string]any `toml:"mcp_servers"`
}

func hostCodexHome() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(user, ".codex")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(home)
}

func readHostMCPConfig(home string) (hostMCPConfig, error) {
	var config hostMCPConfig
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return config, fmt.Errorf("read host Codex MCP configuration: %w", err)
	}
	if err = toml.Unmarshal(b, &config); err != nil {
		return config, fmt.Errorf("cannot parse host Codex MCP configuration")
	}
	return config, nil
}

func fingerprintMCP(home string, server map[string]any) (MCPTrust, error) {
	var trust MCPTrust
	endpoint, _ := server["url"].(string)
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return trust, fmt.Errorf("trusted host MCPs require an HTTPS URL without embedded credentials, query, or fragment")
	}
	if _, ok := server["command"]; ok {
		return trust, fmt.Errorf("host stdio MCP commands cannot be trusted for sandbox use")
	}
	if enabled, ok := server["enabled"].(bool); ok && !enabled {
		return trust, fmt.Errorf("host MCP is disabled")
	}
	b, err := json.Marshal(server)
	if err != nil {
		return trust, fmt.Errorf("cannot fingerprint host MCP configuration")
	}
	hash := sha256.Sum256(b)
	return MCPTrust{URL: endpoint, CodexHome: home, ConfigHash: hex.EncodeToString(hash[:])}, nil
}

// SetRepoMCPTrust also opts the repository into project trust when granting.
// Removing individual MCP grants leaves its project trust decision intact.
func (s *Store) SetRepoMCPTrust(ctx context.Context, nameOrPath string, names []string, trusted bool) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("at least one MCP name is required")
	}
	path, err := ResolveRepoArgument(s.Home, nameOrPath)
	if err != nil {
		return "", err
	}
	repo, err := discoverRepository(ctx, path)
	if err != nil {
		return "", err
	}
	grants := map[string]MCPTrust{}
	for _, name := range names {
		if !aliasName.MatchString(name) {
			return "", fmt.Errorf("invalid MCP name %q", name)
		}
	}
	if trusted {
		home, err := hostCodexHome()
		if err != nil {
			return "", err
		}
		config, err := readHostMCPConfig(home)
		if err != nil {
			return "", err
		}
		for _, name := range names {
			server, ok := config.Servers[name]
			if !ok {
				return "", fmt.Errorf("MCP %q is not configured in host Codex", name)
			}
			grant, err := fingerprintMCP(home, server)
			if err != nil {
				return "", fmt.Errorf("MCP %q: %w", name, err)
			}
			grants[name] = grant
		}
	}
	err = s.withLock(func() error {
		data, err := readRepoConfig(s.Home)
		if err != nil {
			return err
		}
		if trusted {
			if !slices.Contains(data.TrustedRepositories, repo.CommonDir) {
				data.TrustedRepositories = append(data.TrustedRepositories, repo.CommonDir)
			}
			if data.TrustedMCPs == nil {
				data.TrustedMCPs = map[string]map[string]MCPTrust{}
			}
			if data.TrustedMCPs[repo.CommonDir] == nil {
				data.TrustedMCPs[repo.CommonDir] = map[string]MCPTrust{}
			}
		}
		for _, name := range names {
			if trusted {
				data.TrustedMCPs[repo.CommonDir][name] = grants[name]
			} else {
				delete(data.TrustedMCPs[repo.CommonDir], name)
			}
		}
		if len(data.TrustedMCPs[repo.CommonDir]) == 0 {
			delete(data.TrustedMCPs, repo.CommonDir)
		}
		return writeJSONAtomic(filepath.Join(s.Home, "repos.json"), data)
	})
	return repo.CommonDir, err
}

type trustedMCPSet struct {
	home   string
	grants map[string]MCPTrust
	config hostMCPConfig
	names  []string
}

func loadTrustedMCPs(home, commonDir string) (trustedMCPSet, error) {
	var set trustedMCPSet
	data, err := readRepoConfig(home)
	if err != nil {
		return set, err
	}
	if !slices.Contains(data.TrustedRepositories, commonDir) {
		return set, nil
	}
	set.grants = data.TrustedMCPs[commonDir]
	for name, grant := range set.grants {
		if !aliasName.MatchString(name) || !filepath.IsAbs(grant.CodexHome) || len(grant.ConfigHash) != 64 {
			return set, fmt.Errorf("invalid trusted MCP reference")
		}
		if set.home != "" && set.home != grant.CodexHome {
			return set, fmt.Errorf("trusted MCPs use different Codex homes; trust them again with the same CODEX_HOME")
		}
		set.home = grant.CodexHome
		set.names = append(set.names, name)
	}
	if len(set.names) == 0 {
		return set, nil
	}
	sort.Strings(set.names)
	set.config, err = readHostMCPConfig(set.home)
	if err != nil {
		return set, err
	}
	for _, name := range set.names {
		current, err := fingerprintMCP(set.home, set.config.Servers[name])
		if err != nil || current != set.grants[name] {
			return set, fmt.Errorf("trusted MCP %q changed; review the host configuration and run paddock repo trust REPO --mcp %s again", name, name)
		}
	}
	return set, nil
}

func (set trustedMCPSet) authorize(home, commonDir, name string) error {
	grant, ok := set.grants[name]
	if !ok {
		return fmt.Errorf("MCP is not trusted for this task")
	}
	current, err := loadTrustedMCPs(home, commonDir)
	if err != nil {
		return err
	}
	if current.grants[name] != grant {
		return fmt.Errorf("MCP trust was revoked or changed; restart the task after reviewing trust")
	}
	return nil
}
