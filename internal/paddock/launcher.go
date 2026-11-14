package paddock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LauncherConfig describes execution independently of the caller's identity.
// Tasks retain the resolved config so later edits affect only new launches.
type LauncherConfig struct {
	Runtime   string           `json:"runtime,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Terminal  string           `json:"terminal,omitempty"`
	CodexTUI  string           `json:"codex_tui,omitempty"`
	ReadOnly  bool             `json:"read_only,omitempty"`
	OpenShell *OpenShellConfig `json:"openshell,omitempty"`
}

func (c LauncherConfig) validate() error {
	if err := validateCodexTUI(c.CodexTUI); err != nil {
		return err
	}
	if c.Runtime != "" && c.Runtime != "host" && c.Runtime != "openshell" {
		return fmt.Errorf("launcher runtime must be host or openshell")
	}
	if c.Agent != "" && c.Agent != "codex" && c.Agent != "claude" {
		return fmt.Errorf("launcher agent must be codex or claude")
	}
	if c.Terminal != "" && c.Terminal != "auto" && c.Terminal != "rex" && c.Terminal != "screen" {
		return fmt.Errorf("launcher terminal must be auto, rex, or screen")
	}
	if c.Runtime == "host" && (c.ReadOnly || c.OpenShell != nil) {
		return fmt.Errorf("read-only guards and OpenShell settings require the openshell runtime")
	}
	if c.OpenShell != nil {
		return c.OpenShell.validate()
	}
	return nil
}

func (s *Store) LauncherConfigs() (map[string]LauncherConfig, error) {
	// Starter configurations are ordinary defaults, overridable by the operator.
	configs := map[string]LauncherConfig{
		"default":        {},
		"implementation": {Runtime: "openshell"},
		"investigation":  {Runtime: "openshell", ReadOnly: true},
		"host":           {Runtime: "host"},
	}
	b, err := os.ReadFile(filepath.Join(s.Home, "launchers.json"))
	if errors.Is(err, os.ErrNotExist) {
		return configs, nil
	}
	if err != nil {
		return nil, err
	}
	var saved map[string]LauncherConfig
	if err = json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("invalid launchers.json: %w", err)
	}
	for name, c := range saved {
		if !identityName.MatchString(name) {
			return nil, fmt.Errorf("invalid launcher name %q", name)
		}
		if err = c.validate(); err != nil {
			return nil, fmt.Errorf("launcher %s: %w", name, err)
		}
		configs[name] = c
	}
	return configs, nil
}

func (s *Store) LauncherConfig(name string) (LauncherConfig, error) {
	if name == "" {
		name = "default"
	}
	configs, err := s.LauncherConfigs()
	if err != nil {
		return LauncherConfig{}, err
	}
	c, ok := configs[name]
	if !ok {
		return c, fmt.Errorf("unknown launcher %q", name)
	}
	return c, nil
}

func (s *Store) SetLauncherConfig(name string, c LauncherConfig) error {
	if !identityName.MatchString(name) {
		return fmt.Errorf("invalid launcher name %q", name)
	}
	if err := c.validate(); err != nil {
		return err
	}
	return s.withLock(func() error {
		configs, err := s.LauncherConfigs()
		if err != nil {
			return err
		}
		configs[name] = c
		return writeJSONAtomic(filepath.Join(s.Home, "launchers.json"), configs)
	})
}

func (s Service) resolveLauncher(o *RunOptions) error {
	name := o.Launcher
	if name == "" && o.Identity != "" {
		state, err := s.Store.GitHubState()
		if err != nil {
			return err
		}
		name = state.Identities[o.Identity].Launcher
	}
	if name == "" {
		name = "default"
	}
	c, err := s.Store.LauncherConfig(name)
	if err != nil {
		return err
	}
	if o.Runtime == "" {
		o.Runtime = c.Runtime
	}
	if o.Runtime == "" {
		o.Runtime, err = s.Store.DefaultRuntime()
		if err != nil {
			return err
		}
	}
	if o.Agent == "" {
		o.Agent = c.Agent
	}
	if o.Agent == "" {
		o.Agent = "codex"
	}
	if o.Backend == "" {
		o.Backend = c.Terminal
	}
	if o.Backend == "" {
		o.Backend = "auto"
	}
	if o.CodexTUI == "" {
		o.CodexTUI = c.CodexTUI
	}
	c.CodexTUI = o.CodexTUI
	c.Runtime, c.Agent, c.Terminal = o.Runtime, o.Agent, o.Backend
	if err = c.validate(); err != nil {
		return err
	}
	o.Launcher, o.provenance.Launcher, o.launcher = name, name, c
	return nil
}

func (t Task) ReadOnly() bool {
	if t.Launcher != nil {
		return t.Launcher.ReadOnly
	}
	// Existing task records predate configurable launchers.
	return t.Provenance.Profile == "investigation"
}
