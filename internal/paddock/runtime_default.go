package paddock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultRuntime is an explicit operator preference. Missing OpenShell setup
// never causes an implicit fallback to host execution.
func (s *Store) DefaultRuntime() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Home, "runtime.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "openshell", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Default string `json:"default"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		return "", fmt.Errorf("invalid runtime.json: %w", err)
	}
	if config.Default != "openshell" && config.Default != "host" {
		return "", fmt.Errorf("runtime default must be openshell or host")
	}
	return config.Default, nil
}

func (s *Store) SetDefaultRuntime(value string) error {
	if value != "openshell" && value != "host" {
		return fmt.Errorf("runtime default must be openshell or host")
	}
	return s.withLock(func() error {
		return writeJSONAtomic(filepath.Join(s.Home, "runtime.json"), map[string]string{"default": value})
	})
}
