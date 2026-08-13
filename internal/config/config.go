package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Task struct {
	ID                  string   `json:"id"`
	ProcessName         string   `json:"processName"`
	ProcessFilePath     string   `json:"processFilePath"`
	Arguments           []string `json:"arguments,omitempty"`
	ButtonLabel         string   `json:"buttonLabel,omitempty"`
	WindowTitleContains string   `json:"windowTitleContains,omitempty"`
	Enabled             *bool    `json:"enabled,omitempty"`
	IsSystemExtension   bool     `json:"isSystemExtension,omitempty"`
	// When true, the task will only attempt to switch/focus an existing window
	// and will not launch a new process if none is found. Optional.
	SwitchOnly *bool `json:"switchOnly,omitempty"`
	// Optional working directory to set when launching the process
	WorkingDirectory string `json:"workingDirectory,omitempty"`
}

type AppConfig struct {
	SystemLabel     string `json:"systemLabel"`
	Height          int    `json:"height"`
	KeyboardHeight  int    `json:"keyboardHeight"`
	Position        string `json:"position"`        // "top" or "bottom"
	BackgroundColor string `json:"backgroundColor"` // RGB hex color like "#2D2D30"
	TextColor       string `json:"textColor"`       // RGB hex color like "#FFFFFF"
	Tasks           []Task `json:"tasks"`
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

// Default returns the kiosk configuration written when config.json is absent.
func Default() *AppConfig {
	enabled := true
	return &AppConfig{
		SystemLabel:     "TABLET SYSTEM",
		Height:          44,
		KeyboardHeight:  350,
		Position:        "bottom",
		BackgroundColor: "#2D2D30",
		TextColor:       "#FFFFFF",
		Tasks: []Task{
			{
				ID:              "embr",
				ProcessName:     "elwa",
				ProcessFilePath: `C:\embr\elwa\elwa.exe`,
				ButtonLabel:     "EMBR",
				Enabled:         &enabled,
			},
			{
				ID:                  "crestron",
				ProcessName:         "CrestronXPanel",
				ProcessFilePath:     `C:\embr\crestron\GhostDonkeyv1.1.c3p`,
				ButtonLabel:         "CRESTRON",
				Enabled:             &enabled,
				IsSystemExtension:   true,
				WindowTitleContains: "Xpanel",
			},
		},
	}
}

// LoadOrCreate loads path, creating a complete default configuration only
// when the file does not exist. It never replaces an existing file.
func LoadOrCreate(path string) (cfg *AppConfig, created bool, err error) {
	cfg, err = Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	if err := WriteDefault(path); err != nil {
		// Another process may have won the exclusive-create race.
		if os.IsExist(err) {
			cfg, loadErr := Load(path)
			return cfg, false, loadErr
		}
		return nil, false, err
	}
	cfg, err = Load(path)
	return cfg, err == nil, err
}

// WriteDefault writes formatted JSON using exclusive creation so an existing
// operator configuration can never be overwritten.
func WriteDefault(path string) error {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate built-in default config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = f.Close()
		if !written {
			_ = os.Remove(path)
		}
	}()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush default config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close default config: %w", err)
	}
	written = true
	return nil
}

func Load(path string) (*AppConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c AppConfig
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errorsFor("document", "must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("invalid config: trailing data: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *AppConfig) Validate() error {
	if c.Height < 0 {
		return errorsFor("height", "must not be negative")
	}
	if c.KeyboardHeight < 0 {
		return errorsFor("keyboardHeight", "must not be negative")
	}
	if c.Position != "" && c.Position != "top" && c.Position != "bottom" {
		return errorsFor("position", `must be "top" or "bottom"`)
	}
	if err := validateColor("backgroundColor", c.BackgroundColor); err != nil {
		return err
	}
	if err := validateColor("textColor", c.TextColor); err != nil {
		return err
	}
	if len(c.Tasks) == 0 {
		return errorsFor("tasks", "must contain at least one task")
	}
	seenIDs := make(map[string]int, len(c.Tasks))
	for index, task := range c.Tasks {
		name := fmt.Sprintf("tasks[%d]", index)
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return errorsFor(name+".id", "is required")
		}
		normalizedID := strings.ToLower(id)
		if previous, exists := seenIDs[normalizedID]; exists {
			return errorsFor(name+".id", fmt.Sprintf("duplicates tasks[%d].id %q", previous, task.ID))
		}
		seenIDs[normalizedID] = index
		if !task.IsEnabled() {
			continue
		}
		if strings.TrimSpace(task.ButtonLabel) == "" {
			return errorsFor(name+".buttonLabel", "is required for an enabled task")
		}
		if strings.TrimSpace(task.ProcessName) == "" {
			return errorsFor(name+".processName", "is required for an enabled task")
		}
		if !task.IsSwitchOnly() && strings.TrimSpace(task.ProcessFilePath) == "" {
			return errorsFor(name+".processFilePath", "is required unless switchOnly is true")
		}
	}
	return nil
}

func validateColor(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) != 7 || value[0] != '#' {
		return errorsFor(field, `must use RGB form "#RRGGBB"`)
	}
	if _, err := strconv.ParseUint(value[1:], 16, 24); err != nil {
		return errorsFor(field, `must use RGB form "#RRGGBB"`)
	}
	return nil
}

func errorsFor(field, message string) error {
	return fmt.Errorf("invalid config: %s %s", field, message)
}
