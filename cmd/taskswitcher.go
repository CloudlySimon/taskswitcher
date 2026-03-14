//go:build windows

package main

import (
	"log"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"taskswitcher/internal/keyboard"
	"taskswitcher/internal/winutil"
)

func main() {
	a := app.New()
	w := a.NewWindow("Task Switcher")

	label := widget.NewLabel("System")

	cloudBtn := widget.NewButton("Cloud Music System", func() {
		_, hwnd, err := winutil.FindFirstWindowByProcessName("CloudApp")
		if err != nil || hwnd == 0 {
			// fallback: start process
			// _ = exec.Command("CloudApp.exe").Start()
			return
		}
		if winutil.IsIconic(hwnd) {
			winutil.ShowWindow(hwnd, winutil.SW_RESTORE)
		}
		winutil.SetForegroundWindow(hwnd)
	})

	audioBtn := widget.NewButton("Audio Visual System", func() {
		_, hwnd, err := winutil.FindFirstWindowByProcessName("WaveFrontApp")
		if err != nil || hwnd == 0 {
			// _ = exec.Command("WaveFrontApp.exe").Start()
			return
		}
		if winutil.IsIconic(hwnd) {
			winutil.ShowWindow(hwnd, winutil.SW_RESTORE)
		}
		winutil.SetForegroundWindow(hwnd)
	})

	shutdownBtn := widget.NewButton("Shut Down", func() {
		_ = winutil.RunShutdown("/s /t 0")
	})
	restartBtn := widget.NewButton("Restart", func() {
		_ = winutil.RunShutdown("/r /t 0")
	})

	oskBtn := widget.NewButton("On-Screen Keyboard", func() {
		kbd := keyboard.New()
		if err := kbd.Toggle(0, 768, 350, 1024); err != nil {
			log.Println(err)
		}
		go func() {
			for {
				time.Sleep(time.Hour)
			}
		}()
	})

	w.SetContent(container.NewVBox(
		label,
		cloudBtn,
		audioBtn,
		container.NewHBox(shutdownBtn, restartBtn),
		oskBtn,
	))

	w.Resize(fyne.NewSize(600, 200))
	w.ShowAndRun()
}
