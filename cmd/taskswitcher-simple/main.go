//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"taskswitcher/internal/config"
	"taskswitcher/internal/keyboard"
	"taskswitcher/internal/winutil"
)

func main() {
	cfg, err := config.Load(filepath.FromSlash("config.json"))
	if err != nil {
		log.Fatal(err)
	}

	kbd := keyboard.New()
	defer kbd.Close()

	// If command line arguments provided, execute directly
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "cloud":
			launchOrFocus(cfg.CloudTask)
		case "audio":
			launchOrFocus(cfg.WaveFrontTask)
		case "keyboard":
			toggleKeyboard(kbd, cfg)
		case "shutdown":
			exec.Command("shutdown", "/s", "/t", "0").Start()
		case "restart":
			exec.Command("shutdown", "/r", "/t", "0").Start()
		default:
			showHelp()
		}
		return
	}

	// Interactive mode
	for {
		fmt.Printf("\n=== %s Task Switcher ===\n", cfg.SystemLabel)
		fmt.Println("1. Cloud Music System")
		fmt.Println("2. Audio Visual System")
		fmt.Println("3. On-Screen Keyboard")
		fmt.Println("4. Shutdown")
		fmt.Println("5. Restart")
		fmt.Println("6. Exit")
		fmt.Print("Choose option (1-6): ")

		var input string
		fmt.Scanln(&input)

		switch input {
		case "1":
			launchOrFocus(cfg.CloudTask)
		case "2":
			launchOrFocus(cfg.WaveFrontTask)
		case "3":
			toggleKeyboard(kbd, cfg)
		case "4":
			fmt.Print("Confirm shutdown? (y/N): ")
			var confirm string
			fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				exec.Command("shutdown", "/s", "/t", "0").Start()
				return
			}
		case "5":
			fmt.Print("Confirm restart? (y/N): ")
			var confirm string
			fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				exec.Command("shutdown", "/r", "/t", "0").Start()
				return
			}
		case "6":
			return
		default:
			fmt.Println("Invalid option. Please choose 1-6.")
		}
	}
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
		if err := exec.Command(task.ProcessFilePath).Start(); err != nil {
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

func showHelp() {
	fmt.Println("Task Switcher - Command Line Usage:")
	fmt.Println("  taskswitcher-simple.exe cloud     - Launch/focus Cloud Music System")
	fmt.Println("  taskswitcher-simple.exe audio     - Launch/focus Audio Visual System")
	fmt.Println("  taskswitcher-simple.exe keyboard  - Toggle on-screen keyboard")
	fmt.Println("  taskswitcher-simple.exe shutdown  - Shutdown computer")
	fmt.Println("  taskswitcher-simple.exe restart   - Restart computer")
	fmt.Println("  taskswitcher-simple.exe           - Interactive mode")
}
