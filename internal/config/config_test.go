package config

import (
	"os"
	"path/filepath"
	"testing"
)

func boolPointer(value bool) *bool { return &value }

func TestValidateAllowsSwitchOnlyWithoutPath(t *testing.T) {
	cfg := AppConfig{
		Tasks: []Task{{ID: "dsp", ButtonLabel: "DSP", ProcessName: "msedge", SwitchOnly: boolPointer(true)}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsLaunchWithoutPath(t *testing.T) {
	cfg := AppConfig{
		Tasks: []Task{{ID: "elwa", ButtonLabel: "ELWA", ProcessName: "elwa"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsUnknownPosition(t *testing.T) {
	cfg := AppConfig{
		Position: "middle",
		Tasks:    []Task{{ID: "disabled", Enabled: boolPointer(false)}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsInvalidColor(t *testing.T) {
	cfg := AppConfig{
		BackgroundColor: "dark",
		Tasks:           []Task{{ID: "disabled", Enabled: boolPointer(false)}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadRejectsUnknownAndTrailingFields(t *testing.T) {
	tests := map[string]string{
		"unknown":  `{"tasks":[{"id":"one","enabled":false}],"typo":true}`,
		"trailing": `{"tasks":[{"id":"one","enabled":false}]} {}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected load error")
			}
		})
	}
}

func TestLoadTaskArrayPreservesOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{
		"tasks": [
			{"id":"first","buttonLabel":"FIRST","processName":"first","switchOnly":true},
			{"id":"second","buttonLabel":"SECOND","processName":"second","processFilePath":"C:\\apps\\second.exe"}
		]
	}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tasks) != 2 || cfg.Tasks[0].ID != "first" || cfg.Tasks[1].ID != "second" {
		t.Fatalf("unexpected tasks: %#v", cfg.Tasks)
	}
}

func TestLoadRejectsLegacyNamedTaskFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"cloudTask":{"processName":"old"},"waveFrontTask":{"processName":"old"}}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected legacy schema to be rejected")
	}
}

func TestValidateRejectsMissingAndDuplicateTaskIDs(t *testing.T) {
	tests := map[string][]Task{
		"missing":   {{Enabled: boolPointer(false)}},
		"duplicate": {{ID: "DSP", Enabled: boolPointer(false)}, {ID: "dsp", Enabled: boolPointer(false)}},
	}
	for name, tasks := range tests {
		t.Run(name, func(t *testing.T) {
			if err := (&AppConfig{Tasks: tasks}).Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLoadOrCreateWritesValidDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a new default config")
	}
	if len(cfg.Tasks) != 2 || cfg.Tasks[0].ID != "embr" || cfg.Tasks[1].ID != "crestron" {
		t.Fatalf("unexpected default tasks: %#v", cfg.Tasks)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config was not created: %v", err)
	}

	_, created, err = LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("existing config should not be recreated")
	}
}

func TestLoadOrCreateDoesNotReplaceInvalidExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"tasks":`) // deliberately incomplete
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := LoadOrCreate(path); err == nil || created {
		t.Fatalf("expected existing invalid config error, created=%v err=%v", created, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("existing config was changed: %q", after)
	}
}
