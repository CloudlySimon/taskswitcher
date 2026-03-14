//go:build windows

package main

import (
	"log"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"

	"taskswitcher/internal/config"
	"taskswitcher/internal/keyboard"
	"taskswitcher/internal/winutil"
)

type UI struct {
	hwnd windows.HWND
	cfg  *config.AppConfig
	kbd  *keyboard.Keyboard
}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procLoadCursor       = user32.NewProc("LoadCursorW")
	procCreateSolidBrush = windows.NewLazySystemDLL("gdi32.dll").NewProc("CreateSolidBrush")
)

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_VISIBLE          = 0x10000000
	SW_SHOW             = 5
	WM_DESTROY          = 0x0002
	WM_COMMAND          = 0x0111
	COLOR_WINDOW        = 5
	IDC_ARROW           = 32512
)

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type MSG struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

func main() {
	cfg, err := config.Load(filepath.FromSlash("config.json"))
	if err != nil {
		log.Fatal(err)
	}

	ui := &UI{cfg: cfg, kbd: keyboard.New()}
	defer ui.kbd.Close()

	ui.build()
}

func (u *UI) build() {
	height := u.cfg.Height
	if height <= 0 {
		height = 44
	}

	// Create main window directly without declarative syntax to avoid tooltip issues
	var err error
	u.mw, err = walk.NewMainWindow()
	if err != nil {
		log.Printf("Failed to create main window: %v", err)
		return
	}

	u.mw.SetTitle("App Switcher")
	u.mw.SetSize(walk.Size{Width: 1024, Height: height})

	// Create horizontal box layout
	hbox := walk.NewHBoxLayout()
	hbox.SetMargins(walk.Margins{})
	hbox.SetSpacing(0)
	u.mw.SetLayout(hbox)

	// Power button
	powerBtn, err := walk.NewPushButton(u.mw)
	if err == nil {
		powerBtn.SetText("⏻")
		powerBtn.SetMinMaxSize(walk.Size{Width: 48, Height: height}, walk.Size{Width: 48, Height: height})
		powerBtn.Clicked().Attach(u.onPower)
	}

	// System label
	u.label, err = walk.NewLabel(u.mw)
	if err == nil {
		u.label.SetText(u.cfg.SystemLabel)
		u.label.SetTextColor(walk.RGB(255, 255, 255))
		u.label.SetMinMaxSize(walk.Size{Width: 150, Height: height}, walk.Size{Width: 150, Height: height})
	}

	// Cloud button
	cloudBtn, err := walk.NewPushButton(u.mw)
	if err == nil {
		cloudBtn.SetText("CLOUD MUSIC SYSTEM")
		cloudBtn.SetMinMaxSize(walk.Size{Width: 200, Height: height}, walk.Size{Width: 200, Height: height})
		cloudBtn.Clicked().Attach(func() { u.launchOrFocus(u.cfg.CloudTask) })
	}

	// Audio button
	audioBtn, err := walk.NewPushButton(u.mw)
	if err == nil {
		audioBtn.SetText("AUDIO VISUAL SYSTEM")
		audioBtn.SetMinMaxSize(walk.Size{Width: 200, Height: height}, walk.Size{Width: 200, Height: height})
		audioBtn.Clicked().Attach(func() { u.launchOrFocus(u.cfg.WaveFrontTask) })
	}

	// Keyboard button
	kbdBtn, err := walk.NewPushButton(u.mw)
	if err == nil {
		kbdBtn.SetText("⌨")
		kbdBtn.SetMinMaxSize(walk.Size{Width: 100, Height: height}, walk.Size{Width: 100, Height: height})
		kbdBtn.Clicked().Attach(u.onKeyboard)
	}

	u.initChrome(height)
	u.mw.Run()
}

func (u *UI) initChrome(height int) {
	left, _, right, bottom, ok := winutil.GetWorkArea()
	if !ok {
		return
	}
	width := int(right - left)
	u.mw.SetBounds(walk.Rectangle{X: int(left), Y: int(bottom) - height, Width: width, Height: height})
	winutil.SetWindowPos(winutil.HWND(u.mw.Handle()), uintptr(winutil.HWND_TOPMOST), 0, 0, 0, 0, winutil.SWP_NOMOVE|winutil.SWP_NOSIZE|winutil.SWP_NOACTIVATE)
	brush, _ := walk.NewSolidColorBrush(walk.RGB(0, 4, 8))
	u.mw.AsWindowBase().SetBackground(brush)
}

func (u *UI) onPower() {
	dlg, err := walk.NewDialog(u.mw)
	if err != nil {
		return
	}

	dlg.SetTitle("Power")
	dlg.SetSize(walk.Size{Width: 220, Height: 120})

	vbox := walk.NewVBoxLayout()
	vbox.SetMargins(walk.Margins{HNear: 8, VNear: 8, HFar: 8, VFar: 8})
	dlg.SetLayout(vbox)

	shutdownBtn, err := walk.NewPushButton(dlg)
	if err == nil {
		shutdownBtn.SetText("Shut down")
		shutdownBtn.Clicked().Attach(func() {
			exec.Command("shutdown", "/s", "/t", "0").Start()
			dlg.Accept()
		})
	}

	restartBtn, err := walk.NewPushButton(dlg)
	if err == nil {
		restartBtn.SetText("Restart")
		restartBtn.Clicked().Attach(func() {
			exec.Command("shutdown", "/r", "/t", "0").Start()
			dlg.Accept()
		})
	}

	dlg.Run()
}

func (u *UI) onKeyboard() {
	l, _, r, _, ok := winutil.GetWorkArea()
	if !ok {
		return
	}
	width := int(r - l)
	height := u.cfg.KeyboardHeight
	if height <= 0 {
		height = 350
	}
	// Use current window Y as the top of the bar
	bar := u.mw.Bounds()
	_ = u.kbd.Toggle(bar.X, bar.Y, height, width)
}

func (u *UI) launchOrFocus(task config.Task) {
	if task.ProcessName == "" {
		return
	}
	if _, hwnd, err := winutil.FindFirstWindowByProcessName(task.ProcessName); err == nil && hwnd != 0 {
		if winutil.IsIconic(hwnd) {
			winutil.ShowWindow(hwnd, winutil.SW_RESTORE)
		}
		winutil.SetForegroundWindow(hwnd)
		return
	}
	if task.ProcessFilePath != "" {
		_ = exec.Command(task.ProcessFilePath).Start()
		// brief delay then try to bring to front
		time.AfterFunc(1200*time.Millisecond, func() {
			_, hwnd, _ := winutil.FindFirstWindowByProcessName(task.ProcessName)
			if hwnd != 0 {
				winutil.SetForegroundWindow(hwnd)
			}
		})
	}
}
