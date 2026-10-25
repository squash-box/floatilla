package tui

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Settings are per-user preferences that persist between sessions.
type Settings struct {
	// PinLanes (z) keeps empty lanes open.
	PinLanes bool `json:"pin_lanes"`

	path string
}

// LoadSettings reads settings from the user config dir; missing is fine.
func LoadSettings() *Settings {
	s := &Settings{}
	dir, err := os.UserConfigDir()
	if err != nil {
		return s
	}
	s.path = filepath.Join(dir, "floatilla", "tui.json")
	data, err := os.ReadFile(s.path)
	if err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *Settings) Save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.WriteFile(s.path, append(data, '\n'), 0o644)
}
