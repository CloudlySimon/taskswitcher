//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"taskswitcher/internal/config"
	"taskswitcher/internal/keyboard"
	"taskswitcher/internal/winutil"
)

func main() {
	cfg, _, err := config.LoadOrCreate(filepath.FromSlash("config.json"))
	if err != nil {
		log.Fatal(err)
	}

	kbd := keyboard.New()
	defer kbd.Close()

	// If command line arguments provided, execute directly
	if len(os.Args) > 1 {
		command := os.Args[1]
		switch command {
		case "keyboard":
			toggleKeyboard(kbd, cfg)
		case "shutdown":
			exec.Command("shutdown", "/s", "/t", "0").Start()
		case "restart":
			exec.Command("shutdown", "/r", "/t", "0").Start()
		default:
			if task, ok := taskByID(cfg.Tasks, command); ok {
				launchOrFocus(task)
			} else {
				showHelp(cfg)
			}
		}
		return
	}

	// Interactive mode
	for {
		fmt.Printf("\n=== %s Task Switcher ===\n", cfg.SystemLabel)
		tasks := enabledTasks(cfg.Tasks)
		for index, task := range tasks {
			fmt.Printf("%d. %s\n", index+1, task.ButtonLabel)
		}
		keyboardOption := len(tasks) + 1
		shutdownOption := len(tasks) + 2
		restartOption := len(tasks) + 3
		exitOption := len(tasks) + 4
		fmt.Printf("%d. On-Screen Keyboard\n", keyboardOption)
		fmt.Printf("%d. Shutdown\n", shutdownOption)
		fmt.Printf("%d. Restart\n", restartOption)
		fmt.Printf("%d. Exit\n", exitOption)
		fmt.Printf("Choose option (1-%d): ", exitOption)

		var input string
		fmt.Scanln(&input)

		option, err := strconv.Atoi(input)
		if err != nil {
			fmt.Println("Invalid option.")
			continue
		}
		if option >= 1 && option <= len(tasks) {
			launchOrFocus(tasks[option-1])
			continue
		}
		switch option {
		case keyboardOption:
			toggleKeyboard(kbd, cfg)
		case shutdownOption:
			fmt.Print("Confirm shutdown? (y/N): ")
			var confirm string
			fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				exec.Command("shutdown", "/s", "/t", "0").Start()
				return
			}
		case restartOption:
			fmt.Print("Confirm restart? (y/N): ")
			var confirm string
			fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				exec.Command("shutdown", "/r", "/t", "0").Start()
				return
			}
		case exitOption:
			return
		default:
			fmt.Printf("Invalid option. Please choose 1-%d.\n", exitOption)
		}
	}
}

func enabledTasks(tasks []config.Task) []config.Task {
	enabled := make([]config.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.IsEnabled() {
			enabled = append(enabled, task)
		}
	}
	return enabled
}

func taskByID(tasks []config.Task, id string) (config.Task, bool) {
	for _, task := range tasks {
		if strings.EqualFold(task.ID, id) && task.IsEnabled() {
			return task, true
		}
	}
	return config.Task{}, false
}

func launchOrFocus(task config.Task) {
	if task.ProcessName == "" {
		fmt.Println("Task not configured")
		return
	}

	fmt.Printf("Looking for %s...\n", task.ProcessName)

	if _, hwnd, err := winutil.FindFirstWindowByProcessName(task.ProcessName); err == nil && hwnd != 0 {
		fmt.Printf("Found %s, bringing to front...\n", task.ProcessName)
		if winutil.IsIconic(hwnd) {
			winutil.ShowWindow(hwnd, winutil.SW_RESTORE)
		}
		winutil.SetForegroundWindow(hwnd)
		return
	}

	if task.ProcessFilePath != "" {
		fmt.Printf("Starting %s...\n", task.ProcessName)
		cmd := exec.Command(task.ProcessFilePath, task.Arguments...)
		if task.WorkingDirectory != "" {
			cmd.Dir = task.WorkingDirectory
		}
		if err := cmd.Start(); err != nil {
			fmt.Printf("Failed to start %s: %v\n", task.ProcessName, err)
			return
		}
		fmt.Printf("%s started successfully\n", task.ProcessName)
	} else {
		fmt.Printf("Process %s not found and no launch path configured\n", task.ProcessName)
	}
}

func toggleKeyboard(kbd *keyboard.Keyboard, cfg *config.AppConfig) {
	left, _, right, bottom, ok := winutil.GetWorkArea()
	if !ok {
		fmt.Println("Failed to get work area")
		return
	}

	width := int(right - left)
	height := cfg.KeyboardHeight
	if height <= 0 {
		height = 350
	}

	fmt.Println("Toggling on-screen keyboard...")
	if err := kbd.Toggle(int(left), int(bottom)-height, height, width); err != nil {
		fmt.Printf("Failed to toggle keyboard: %v\n", err)
	} else {
		fmt.Println("Keyboard toggled")
	}
}

func showHelp(cfg *config.AppConfig) {
	fmt.Println("Task Switcher - Command Line Usage:")
	for _, task := range enabledTasks(cfg.Tasks) {
		fmt.Printf("  taskswitcher-simple.exe %-10s - Launch/focus %s\n", task.ID, task.ButtonLabel)
	}
	fmt.Println("  taskswitcher-simple.exe keyboard  - Toggle on-screen keyboard")
	fmt.Println("  taskswitcher-simple.exe shutdown  - Shutdown computer")
	fmt.Println("  taskswitcher-simple.exe restart   - Restart computer")
	fmt.Println("  taskswitcher-simple.exe           - Interactive mode")
}
