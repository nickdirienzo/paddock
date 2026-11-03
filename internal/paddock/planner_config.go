package paddock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type PlannerConfig struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions"`
}

func (s *Store) PlannerConfig() (PlannerConfig, error) {
	var config PlannerConfig
	data, err := os.ReadFile(filepath.Join(s.Home, "planner.json"))
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &config)
	}
	return config, err
}
func (s *Store) SetPlannerConfig(config PlannerConfig) error {
	if len(config.Instructions) > 16000 || len(config.Model) > 200 {
		return fmt.Errorf("planner configuration exceeds size limits")
	}
	return s.withLock(func() error { return writeJSONAtomic(filepath.Join(s.Home, "planner.json"), config) })
}
