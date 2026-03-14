package config

import (
	"encoding/json"
	"os"
)

type Task struct {
	ProcessName     string   `json:"processName"`
	ProcessFilePath string   `json:"processFilePath"`
	Arguments       []string `json:"arguments,omitempty"`
	ButtonLabel     string   `json:"buttonLabel,omitempty"`
	WindowTitleContains string `json:"windowTitleContains,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	IsSystemExtension bool   `json:"isSystemExtension,omitempty"`
	// When true, the task will only attempt to switch/focus an existing window
	// and will not launch a new process if none is found. Optional.
	SwitchOnly      *bool    `json:"switchOnly,omitempty"`
	// Optional working directory to set when launching the process
	WorkingDirectory string  `json:"workingDirectory,omitempty"`
}

type AppConfig struct {
	SystemLabel    string `json:"systemLabel"`
	Height         int    `json:"height"`
	KeyboardHeight int    `json:"keyboardHeight"`
	Position       string `json:"position"`       // "top" or "bottom"
	BackgroundColor string `json:"backgroundColor"` // RGB hex color like "#2D2D30"
	TextColor      string `json:"textColor"`       // RGB hex color like "#FFFFFF"
	CloudTask      Task   `json:"cloudTask"`
	WaveFrontTask  Task   `json:"waveFrontTask"`
}

// IsEnabled returns true if the task is enabled (default true if not specified)
func (t *Task) IsEnabled() bool {
	if t.Enabled == nil {
		return true // Default to enabled if not specified
	}
	return *t.Enabled
}

// IsSwitchOnly returns true if the task should only switch/focus an existing window
// (default false if not specified)
func (t *Task) IsSwitchOnly() bool {
	if t.SwitchOnly == nil {
		return false
	}
	return *t.SwitchOnly
}

func Load(path string) (*AppConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c AppConfig
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
