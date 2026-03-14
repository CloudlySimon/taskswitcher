//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"taskswitcher/internal/config"
	"taskswitcher/internal/winutil"

	"golang.org/x/sys/windows"
)

// Build-time visible version of the toolbar binary. Keep in sync with version.txt at repo root.
const AppVersion = "0.1.4-dev"

type UI struct {
	hwnd            windows.HWND
	cfg             *config.AppConfig
	mu              sync.Mutex
	processing      bool
	processingSince time.Time
	lastLaunch      map[string]time.Time
	buttonHWNDs     map[int]windows.HWND
	pendingTask     string
	// Tracks last time we accepted a BN_CLICKED per button id (UI thread only)
	lastCmdAt map[int]time.Time
	// Tracks last time a WM_TIMER heartbeat was observed (for watchdog)
	lastTimerAt time.Time
}

var (
	// Windows API functions
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procPeekMessage      = user32.NewProc("PeekMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procLoadCursor       = user32.NewProc("LoadCursorW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procPostMessage      = user32.NewProc("PostMessageW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow2      = user32.NewProc("ShowWindow")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procSetTextColor     = gdi32.NewProc("SetTextColor")
	procSetTimer         = user32.NewProc("SetTimer")
	procIsWindowEnabled  = user32.NewProc("IsWindowEnabled")
	procEnableWindow     = user32.NewProc("EnableWindow")
	procSetWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLongPtr = user32.NewProc("GetWindowLongPtrW")
	procCallWindowProc   = user32.NewProc("CallWindowProcW")
	procSendMessage      = user32.NewProc("SendMessageW")
	procShellExecute     = shell32.NewProc("ShellExecuteW")
)

const (
	// Windows constants
	WS_POPUP         = 0x80000000
	WS_VISIBLE       = 0x10000000
	WS_CHILD         = 0x40000000
	WS_BORDER        = 0x00800000
	WS_EX_TOPMOST    = 0x00000008
	WS_EX_TOOLWINDOW = 0x00000080
	BS_PUSHBUTTON    = 0x00000000
	BS_NOTIFY        = 0x00004000
	WS_TABSTOP       = 0x00010000
	SW_SHOW          = 5
	SW_HIDE          = 0
	IDC_ARROW        = 32512
	WM_COMMAND       = 0x0111
	WM_TIMER         = 0x0113
	WM_CLOSE         = 0x0010
	WM_DESTROY       = 0x0002
	WM_SYSKEYDOWN    = 0x0104
	VK_F4            = 0x73
	SM_CXSCREEN      = 0
	SM_CYSCREEN      = 1
	WM_LBUTTONUP     = 0x0202
	WM_LBUTTONDOWN   = 0x0201
	WM_LBUTTONDBLCLK = 0x0203
	WM_SETCURSOR     = 0x0020
	BN_CLICKED       = 0
	// Button notifications (HIWORD(wParam))
	BN_PAINT         = 1
	BN_HILITE        = 2
	BN_UNHILITE      = 3
	BN_DISABLE       = 4
	BN_DOUBLECLICKED = 5
	BN_SETFOCUS      = 6
	BN_KILLFOCUS     = 7

	// Button IDs
	ID_CLOUD_BTN = 1001
	ID_AUDIO_BTN = 1002
	
	// ShellExecute constants
	SW_SHOWNORMAL = 1
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

var globalUI *UI
var debugFile *os.File

var (
	btnOrigProc   = make(map[windows.HWND]uintptr)
	hwndToID      = make(map[windows.HWND]int)
	btnWndProcPtr uintptr
)

// Use a typed variable for GWLP_WNDPROC to allow conversion to uintptr without constant overflow
var gwlpWndProc int32 = -4

func debugLog(msg string) {
	if debugFile != nil {
		timestamp := time.Now().Format("2006-01-02 15:04:05.000")
		fmt.Fprintf(debugFile, "[%s] %s\n", timestamp, msg)
		debugFile.Sync() // Force write to disk
	}
}

// readVersionFile reads version.txt from the project root (working directory) to assist
// operators in confirming that the running binary matches the intended version.
func readVersionFile() string {
	// 1) Try current working directory
	if b, err := os.ReadFile("version.txt"); err == nil {
		return strings.TrimSpace(string(b))
	} else {
		log.Printf("readVersionFile: not found at CWD: %s", filepath.Join(".", "version.txt"))
	}
	// 2) Try alongside the executable (common when started from other working dirs)
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		path := filepath.Join(dir, "version.txt")
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		} else {
			log.Printf("readVersionFile: not found next to exe: %s", path)
		}
	} else {
		log.Printf("readVersionFile: os.Executable() failed: %v", err)
	}
	return ""
}

func hideConsoleWindow() {
	// Get console window handle
	consoleWindow, _, _ := procGetConsoleWindow.Call()
	if consoleWindow != 0 {
		// Hide the console window
		procShowWindow.Call(consoleWindow, SW_HIDE)
	}
}

func main() {
	// Flags
	var configPath string
	var debugEnabled bool
	flag.StringVar(&configPath, "config", "config.json", "Path to config file")
	flag.BoolVar(&debugEnabled, "debug", false, "Enable debug logging to taskswitcher-toolbar-debug.log")
	flag.Parse()

	// Configure logging based on debug flag
	if debugEnabled {
		var err error
		debugFile, err = os.OpenFile("taskswitcher-toolbar-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			// If we can't create log file, continue without it
			debugFile = nil
		}
		if debugFile != nil {
			log.SetOutput(debugFile)
		}
		log.Printf("=== TOOLBAR APPLICATION STARTING ===")
		// Print binary version and compare with version.txt
		log.Printf("Toolbar AppVersion: %s", AppVersion)
		if vf := readVersionFile(); vf != "" {
			if vf == AppVersion {
				log.Printf("version.txt verified: %s", vf)
			} else {
				log.Printf("WARNING: version.txt=%q differs from binary AppVersion=%q", vf, AppVersion)
			}
		} else {
			log.Printf("version.txt not found or unreadable; proceeding")
		}
		log.Printf("Loading config from: %s", configPath)
	} else {
		// Discard logs by default when not in debug mode
		log.SetOutput(io.Discard)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Config loaded successfully:")
	log.Printf("  CloudTask.ProcessName: '%s'", cfg.CloudTask.ProcessName)
	log.Printf("  CloudTask.ButtonLabel: '%s'", cfg.CloudTask.ButtonLabel)
	log.Printf("  WaveFrontTask.ProcessName: '%s'", cfg.WaveFrontTask.ProcessName)
	log.Printf("  WaveFrontTask.ButtonLabel: '%s'", cfg.WaveFrontTask.ButtonLabel)

	// Hide console window
	hideConsoleWindow()

	log.Printf("Creating UI instance...")
	ui := &UI{cfg: cfg, lastLaunch: make(map[string]time.Time), buttonHWNDs: make(map[int]windows.HWND), processingSince: time.Time{}, lastCmdAt: make(map[int]time.Time), lastTimerAt: time.Time{}}
	globalUI = ui // Set global reference for window procedure
	log.Printf("Global UI reference set")
	log.Printf("Starting UI run...")
	ui.run()

	// Cleanup
	if debugFile != nil {
		debugFile.Close()
	}
}

func (u *UI) run() {
	// Ensure the window is created and message loop runs on the same OS thread.
	// This is required for Win32 GUI correctness (message queue is per-thread).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	log.Printf("=== UI.run() STARTING ===")

	// Get module handle
	log.Printf("Step 1: Getting module handle...")
	hInstance, _, _ := procGetModuleHandle.Call(0)
	log.Printf("Step 1 COMPLETE: Got module handle: %d", hInstance)

	// Register window class
	log.Printf("Step 2: Creating window class strings...")
	className, _ := syscall.UTF16PtrFromString("TaskSwitcherToolbar")
	log.Printf("Step 2a: Window class name created")

	log.Printf("Step 2b: Loading cursor...")
	cursor, _, _ := procLoadCursor.Call(0, IDC_ARROW)
	log.Printf("Step 2b COMPLETE: Cursor loaded: %d", cursor)

	// Create background brush with configurable color
	log.Printf("Step 3: Creating background brush...")
	var brushColor uintptr = 0x2D2D30 // Default dark gray
	if u.cfg.BackgroundColor != "" {
		log.Printf("Step 3a: Parsing custom background color: %s", u.cfg.BackgroundColor)
		r, g, b := u.parseHexColor(u.cfg.BackgroundColor)
		brushColor = uintptr(r) | (uintptr(g) << 8) | (uintptr(b) << 16)
		log.Printf("Step 3a COMPLETE: Parsed color RGB(%d,%d,%d) = %d", r, g, b, brushColor)
	}
	log.Printf("Step 3b: Creating solid brush...")
	brush, _, _ := procCreateSolidBrush.Call(brushColor)
	log.Printf("Step 3b COMPLETE: Brush created: %d", brush)

	log.Printf("Step 4: Creating WNDCLASSEX structure...")
	wc := WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEX{})),
		LpfnWndProc:   syscall.NewCallback(wndProc),
		HInstance:     windows.Handle(hInstance),
		HCursor:       windows.Handle(cursor),
		HbrBackground: windows.Handle(brush),
		LpszClassName: className,
	}
	log.Printf("Step 4 COMPLETE: WNDCLASSEX structure created")

	log.Printf("Step 5: Registering window class...")
	atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		log.Fatal("Failed to register window class")
	}
	log.Printf("Step 5 COMPLETE: Window class registered successfully, atom=%d", atom)

	// Get work area dimensions
	log.Printf("Step 6: Getting work area dimensions...")
	left, top, right, bottom, ok := winutil.GetWorkArea()
	if !ok {
		log.Fatal("Failed to get work area")
	}
	width := right - left
	log.Printf("Step 6 COMPLETE: Work area: left=%d, top=%d, right=%d, bottom=%d, width=%d", left, top, right, bottom, width)

	log.Printf("Step 7: Calculating window dimensions...")
	height := u.cfg.Height
	if height <= 0 {
		height = 44
	}

	var y int32
	if u.cfg.Position == "top" {
		y = top
	} else {
		y = bottom - int32(height) // default to bottom
	}
	log.Printf("Step 7 COMPLETE: Window dimensions: height=%d, y=%d", height, y)

	// Create window
	log.Printf("Step 8: Creating main window...")
	windowName, _ := syscall.UTF16PtrFromString("Task Switcher")
	hwnd, _, _ := procCreateWindowEx.Call(
		WS_EX_TOPMOST|WS_EX_TOOLWINDOW,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		WS_POPUP|WS_VISIBLE|WS_BORDER,
		uintptr(left),
		uintptr(y),
		uintptr(width),
		uintptr(height),
		0, 0,
		hInstance,
		0,
	)

	if hwnd == 0 {
		log.Fatal("Failed to create window")
	}
	log.Printf("Step 8 COMPLETE: Main window created successfully, hwnd=%d", hwnd)

	u.hwnd = windows.HWND(hwnd)

	// Create buttons
	log.Printf("Step 9: Creating buttons...")
	u.createButtons()
	log.Printf("Step 9 COMPLETE: Buttons created")

	// Show window first
	log.Printf("Step 10: Showing window...")
	showResult, _, _ := procShowWindow.Call(uintptr(u.hwnd), SW_SHOW)
	log.Printf("Step 10a: ShowWindow result: %d", showResult)
	updateResult, _, _ := procUpdateWindow.Call(uintptr(u.hwnd))
	log.Printf("Step 10b: UpdateWindow result: %d", updateResult)
	
	// Ensure window maintains correct size and position after showing
	log.Printf("Step 10c: Ensuring correct window size and position...")
	setResult, _, _ := procSetWindowPos.Call(
		uintptr(u.hwnd),
		uintptr(0xFFFFFFFF), // HWND_TOPMOST
		uintptr(left),
		uintptr(y),
		uintptr(width),
		uintptr(height),
		0x0040, // SWP_SHOWWINDOW
	)
	log.Printf("Step 10c: SetWindowPos result: %d", setResult)
	log.Printf("Step 10 COMPLETE: Window display calls completed")

	// Start a heartbeat timer to detect UI stalls (1s interval)
	if u.hwnd != 0 {
		if tid, _, _ := procSetTimer.Call(uintptr(u.hwnd), 1, 1000, 0); tid == 0 {
			log.Printf("Failed to start heartbeat timer")
		} else {
			log.Printf("Heartbeat timer started (id=1, 1000ms)")
			// Post a one-time synthetic WM_TIMER to validate delivery path and logging
			procPostMessage.Call(uintptr(u.hwnd), uintptr(WM_TIMER), 1, 0)
			log.Printf("Posted self-test WM_TIMER (id=1)")
			// Initialize lastTimerAt to now since timer is armed
			u.mu.Lock()
			u.lastTimerAt = time.Now()
			u.mu.Unlock()
		}
	}

	// Start a secondary Go ticker heartbeat to confirm process liveness even if WM_TIMER stops
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// Snapshot state under lock
			u.mu.Lock()
			proc := u.processing
			lastT := u.lastTimerAt
			hwnd := u.hwnd
			u.mu.Unlock()

			log.Printf("GoTicker heartbeat: processing=%v", proc)

			// Watch for missing WM_TIMER delivery and attempt recovery
			if !lastT.IsZero() {
				gap := time.Since(lastT)
				if gap > 2500*time.Millisecond && hwnd != 0 {
					log.Printf("GoTicker: detected WM_TIMER gap of %v (>2.5s); re-arming timer and posting synthetic WM_TIMER", gap)
					// Re-arm the window timer (id=1) and post a synthetic WM_TIMER
					if tid, _, _ := procSetTimer.Call(uintptr(hwnd), 1, 1000, 0); tid == 0 {
						log.Printf("GoTicker: SetTimer re-arm failed")
					} else {
						procPostMessage.Call(uintptr(hwnd), uintptr(WM_TIMER), 1, 0)
					}
				}
			}
		}
	}()

	// Start message loop (blocking)
	log.Printf("Step 11: Starting message loop (BLOCKING)...")
	log.Printf("=== APPLICATION STARTUP COMPLETE ===")
	u.messageLoop()
	log.Printf("Step 11 COMPLETE: Message loop ended - application shutting down")
}

func (u *UI) messageLoop() {
	log.Printf("=== MESSAGE LOOP STARTING ===")
	var msg MSG
	msgCount := 0
	for {
		ret, _, _ := procGetMessage.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)
		if int32(ret) == -1 { // error
			log.Printf("GetMessage failed; exiting message loop")
			break
		}
		if ret == 0 { // WM_QUIT
			log.Printf("WM_QUIT received, exiting message loop")
			break
		}

		msgCount++
		if (msgCount%100 == 1 && msg.Message != WM_SETCURSOR) || msg.Message == WM_COMMAND || msg.Message == WM_DESTROY || msg.Message == WM_TIMER {
			log.Printf("Processing message %d: type=%d, wParam=%d, lParam=%d", msgCount, msg.Message, msg.WParam, msg.LParam)
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	log.Printf("=== MESSAGE LOOP ENDED ===")
}

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("wndProc: recovered from panic: %v (msg=%d, wParam=%d, lParam=%d)", r, msg, wParam, lParam)
		}
	}()
	switch msg {
	case WM_COMMAND:
		cmdID := int(wParam & 0xFFFF)
		notifyCode := (wParam >> 16) & 0xFFFF
		// Diagnostics: WM_COMMAND from child controls delivers lParam = child HWND
		log.Printf("WM_COMMAND received: id=%d, notify=%d, childHWND=%d", cmdID, notifyCode, lParam)

		// Check if globalUI is initialized
		if globalUI == nil {
			return 0
		}
		// Only handle actual clicks; ignore focus/paint notifications
		if notifyCode != BN_CLICKED {
			return 0
		}
		// Verify that the sender HWND matches the registered button handle for this id
		if hwndBtn, ok := globalUI.buttonHWNDs[cmdID]; ok {
			if uintptr(hwndBtn) != lParam {
				// Not from our known button; ignore
				return 0
			}
		} else {
			// Unknown command id
			return 0
		}
		// De-duplicate double-delivered BN_CLICKED (subclass + native) within a short window
		if globalUI.lastCmdAt != nil {
			now := time.Now()
			if t, ok := globalUI.lastCmdAt[cmdID]; ok {
				if now.Sub(t) < 90*time.Millisecond {
					log.Printf("WM_COMMAND: duplicate BN_CLICKED for id=%d ignored (delta=%v < 90ms)", cmdID, now.Sub(t))
					return 0
				}
			}
			globalUI.lastCmdAt[cmdID] = now
		}

		switch cmdID {
		case ID_CLOUD_BTN:
			globalUI.safeButtonClick(globalUI.cfg.CloudTask.ProcessName)
		case ID_AUDIO_BTN:
			globalUI.safeButtonClick(globalUI.cfg.WaveFrontTask.ProcessName)
		}
		return 0
	case WM_TIMER:
		// UI heartbeat to verify message processing during potential stalls
		procState := false
		age := time.Duration(0)
		pending := ""
		if globalUI != nil {
			globalUI.mu.Lock()
			// Record last WM_TIMER arrival time for watchdog logic
			globalUI.lastTimerAt = time.Now()
			procState = globalUI.processing
			if procState && !globalUI.processingSince.IsZero() {
				age = time.Since(globalUI.processingSince)
			}
			pending = globalUI.pendingTask
			globalUI.mu.Unlock()
		}
		log.Printf("WM_TIMER heartbeat: id=%d, processing=%v, age=%v, pendingTask=%q", wParam, procState, age, pending)
		// Watchdog: if processing appears stuck for >3s, clear it
		if globalUI != nil {
			globalUI.mu.Lock()
			if globalUI.processing && !globalUI.processingSince.IsZero() {
				if time.Since(globalUI.processingSince) > 3*time.Second {
					log.Printf("Watchdog: clearing stuck processing state (age=%v)", time.Since(globalUI.processingSince))
					globalUI.processing = false
				}
			}
			// Drain a single pending task if we're idle now
			var toLaunch string
			if !globalUI.processing && globalUI.pendingTask != "" {
				toLaunch = globalUI.pendingTask
				globalUI.pendingTask = ""
				log.Printf("WM_TIMER: draining pendingTask=%s", toLaunch)
			}
			// Copy button HWNDs to check enable state outside the lock
			handles := make([]windows.HWND, 0, len(globalUI.buttonHWNDs))
			for _, h := range globalUI.buttonHWNDs {
				handles = append(handles, h)
			}
			globalUI.mu.Unlock()

			if toLaunch != "" {
				// Call outside of lock to avoid deadlock
				globalUI.safeButtonClick(toLaunch)
			}
			// Ensure buttons are enabled (Windows can disable after certain interactions)
			for _, h := range handles {
				if h == 0 {
					continue
				}
				enabled, _, _ := procIsWindowEnabled.Call(uintptr(h))
				if enabled == 0 {
					log.Printf("WM_TIMER: button hwnd=%d was disabled; re-enabling", h)
					procEnableWindow.Call(uintptr(h), 1)
				}
			}
		}
		return 0
	case WM_DESTROY:
		log.Printf("WM_DESTROY received")
		procPostQuitMessage.Call(0)
		return 0
	case WM_SYSKEYDOWN:
		log.Printf("WM_SYSKEYDOWN received: key=%d", wParam)
		// Let DefWindowProc handle system keys
	default:
		// Only log unusual messages
		if msg != 15 && msg != 20 && msg != 132 && msg != 30 && msg != 512 {
			log.Printf("Window message: %d (wParam=%d, lParam=%d)", msg, wParam, lParam)
		}
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func btnWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("btnWndProc: recovered from panic: %v (msg=%d, wParam=%d, lParam=%d)", r, msg, wParam, lParam)
		}
	}()
	// Extra mouse diagnostics
	if msg == WM_LBUTTONDOWN {
		log.Printf("btnWndProc: WM_LBUTTONDOWN hwnd=%d", hwnd)
	}
	if msg == WM_LBUTTONDBLCLK {
		// Do not synthesize BN_CLICKED here; treat as double-click diagnostic only
		log.Printf("btnWndProc: WM_LBUTTONDBLCLK hwnd=%d", hwnd)
	}
	// On mouse up over the button, ensure BN_CLICKED reaches parent
	if msg == WM_LBUTTONUP {
		log.Printf("btnWndProc: WM_LBUTTONUP hwnd=%d", hwnd)
		if globalUI != nil {
			id, ok := hwndToID[windows.HWND(hwnd)]
			if ok {
				// Compose WPARAM = MAKEWPARAM(id, BN_CLICKED)
				w := uintptr(uint32(id)&0xFFFF | (uint32(BN_CLICKED)&0xFFFF)<<16)
				// lParam = child HWND
				log.Printf("btnWndProc: WM_LBUTTONUP -> synthesize BN_CLICKED for id=%d, hwnd=%d", id, hwnd)
				// Use PostMessage to avoid reentrancy into parent wndProc
				procPostMessage.Call(uintptr(globalUI.hwnd), uintptr(WM_COMMAND), w, hwnd)
			}
		}
	}
	// Call original button proc
	if prev, ok := btnOrigProc[windows.HWND(hwnd)]; ok && prev != 0 {
		ret, _, _ := procCallWindowProc.Call(prev, hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
	// Fallback to DefWindowProc if we somehow lost the original
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func (u *UI) createButtons() {
	height := u.cfg.Height
	if height <= 0 {
		height = 44
	}

	// Get screen dimensions for centering
	screenWidth, _, _ := procGetSystemMetrics.Call(SM_CXSCREEN)

	// Calculate button dimensions with modern styling
	buttonHeight := height - 12 // More padding
	buttonSpacing := 16

	// Only create buttons that are configured and enabled
	var buttons []buttonInfo

	// Cloud Music System button
	if u.cfg.CloudTask.ProcessName != "" && u.cfg.CloudTask.IsEnabled() {
		buttonText := u.cfg.CloudTask.ButtonLabel
		log.Printf("DEBUG: CloudTask.ButtonLabel = '%s'", buttonText)
		if buttonText == "" {
			buttonText = "CLOUD MUSIC SYSTEM"
			log.Printf("DEBUG: Using fallback button text: '%s'", buttonText)
		}
		buttons = append(buttons, buttonInfo{
			text:  buttonText,
			width: 220,
			id:    ID_CLOUD_BTN,
		})
	}

	// Audio Visual System button
	if u.cfg.WaveFrontTask.ProcessName != "" && u.cfg.WaveFrontTask.IsEnabled() {
		buttonText := u.cfg.WaveFrontTask.ButtonLabel
		log.Printf("DEBUG: WaveFrontTask.ButtonLabel = '%s'", buttonText)
		if buttonText == "" {
			buttonText = "AUDIO VISUAL SYSTEM"
			log.Printf("DEBUG: Using fallback button text: '%s'", buttonText)
		}
		buttons = append(buttons, buttonInfo{
			text:  buttonText,
			width: 220,
			id:    ID_AUDIO_BTN,
		})
	}

	// Calculate total width and center the buttons
	totalWidth := 0
	for i, btn := range buttons {
		totalWidth += btn.width
		if i < len(buttons)-1 {
			totalWidth += buttonSpacing
		}
	}

	startX := (int(screenWidth) - totalWidth) / 2
	x := startX

	// Create the buttons
	for _, btn := range buttons {
		u.createStyledButton(btn.text, x, 6, btn.width, buttonHeight, btn.id)
		x += btn.width + buttonSpacing
	}
}

type buttonInfo struct {
	text    string
	width   int
	id      int
	isLabel bool
}

func (u *UI) createStyledButton(text string, x, y, width, height int, id int) {
	buttonText, _ := syscall.UTF16PtrFromString(text)
	className, _ := syscall.UTF16PtrFromString("BUTTON")
	hInstance, _, _ := procGetModuleHandle.Call(0)
	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(buttonText)),
		WS_VISIBLE|WS_CHILD|BS_PUSHBUTTON|WS_TABSTOP|BS_NOTIFY, // Added BS_NOTIFY for command messages
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		uintptr(u.hwnd),
		uintptr(id),
		hInstance, // Proper instance handle
		0,
	)
	if hwnd != 0 {
		log.Printf("Button created: hwnd=%d, id=%d, text='%s'", hwnd, id, text)
		if u.buttonHWNDs != nil {
			u.buttonHWNDs[id] = windows.HWND(hwnd)
		}
		// Track reverse map and subclass to ensure clicks are delivered
		hwndToID[windows.HWND(hwnd)] = id
		if btnWndProcPtr == 0 {
			btnWndProcPtr = syscall.NewCallback(btnWndProc)
		}
		prev, _, _ := procSetWindowLongPtr.Call(hwnd, uintptr(gwlpWndProc), btnWndProcPtr)
		if prev != 0 {
			btnOrigProc[windows.HWND(hwnd)] = prev
			log.Printf("Subclassed button hwnd=%d (id=%d), prevProc=%#x", hwnd, id, prev)
		} else {
			log.Printf("WARNING: failed to subclass button hwnd=%d (id=%d)", hwnd, id)
		}
		if u.cfg.BackgroundColor != "" || u.cfg.TextColor != "" {
			u.applyButtonColors(windows.HWND(hwnd))
		}
	} else {
		log.Printf("Failed to create button: id=%d, text='%s'", id, text)
	}
}

// Helper function to parse hex color to RGB
func (u *UI) parseHexColor(hex string) (r, g, b uint8) {
	if len(hex) != 7 || hex[0] != '#' {
		return 45, 45, 48 // default dark gray
	}

	val, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 45, 45, 48 // default dark gray
	}

	r = uint8((val >> 16) & 0xFF)
	g = uint8((val >> 8) & 0xFF)
	b = uint8(val & 0xFF)
	return
}

// Apply custom colors to button (basic implementation)
func (u *UI) applyButtonColors(hwnd windows.HWND) {
	// Note: Full color customization would require custom drawing
	// This is a placeholder for the color functionality
}

func (u *UI) createStyledLabel(text string, x, y, width, height int) {
	labelText, _ := syscall.UTF16PtrFromString(text)
	className, _ := syscall.UTF16PtrFromString("STATIC")

	// Create centered label with modern styling
	procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(labelText)),
		0x50000000|0x00000001|0x00000200, // WS_VISIBLE | WS_CHILD | SS_CENTER | SS_CENTERIMAGE
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		uintptr(u.hwnd),
		0, 0, 0,
	)
}

func (u *UI) safeButtonClick(processName string) {
	u.mu.Lock()
	log.Printf("safeButtonClick: requested for %s (processing=%v)", processName, u.processing)
	// Debounce rapid repeated clicks per task (200ms window)
	now := time.Now()
	if t, ok := u.lastLaunch[processName]; ok {
		if elapsed := now.Sub(t); elapsed < 200*time.Millisecond {
			log.Printf("safeButtonClick: debounce - ignoring duplicate for %s (elapsed=%v < 200ms)", processName, elapsed)
			// If we're idle and have no pending task, schedule a short delayed fire to keep UX responsive
			if !u.processing && u.pendingTask == "" {
				u.pendingTask = processName
				log.Printf("safeButtonClick: scheduled pendingTask=%s after debounce (delayed fire)", processName)
				// Capture UI pointer safely
				ui := u
				time.AfterFunc(120*time.Millisecond, func() {
					ui.mu.Lock()
					toLaunch := ""
					if !ui.processing && ui.pendingTask != "" {
						toLaunch = ui.pendingTask
						ui.pendingTask = ""
						log.Printf("debounce-delayed: firing pendingTask=%s", toLaunch)
					}
					ui.mu.Unlock()
					if toLaunch != "" {
						ui.safeButtonClick(toLaunch)
					}
				})
			}
			u.mu.Unlock()
			return
		}
	}

	if u.processing {
		// Queue a single pending task so we don't lose a click
		if u.pendingTask == "" {
			u.pendingTask = processName
			log.Printf("safeButtonClick: already processing, queued pendingTask=%s", processName)
		} else {
			log.Printf("safeButtonClick: already processing, pendingTask exists=%s; ignoring %s", u.pendingTask, processName)
		}
		u.mu.Unlock()
		return // Already processing, ignore click
	}
	// Mark processing and record timestamp, then release the lock before heavy work
	u.processing = true
	u.processingSince = now
	u.lastLaunch[processName] = now
	u.mu.Unlock()

	go func() {
		start := time.Now()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("safeButtonClick: panic during launch of %s: %v", processName, r)
			}
			u.mu.Lock()
			u.processing = false
			u.mu.Unlock()
			log.Printf("safeButtonClick: completed %s in %v", processName, time.Since(start))
		}()
		log.Printf("safeButtonClick: launching %s now", processName)
		u.simpleLaunch(processName)
	}()
}

func bringWindowToFront(hwnd winutil.HWND) {
	// If window is minimized, restore it
	if winutil.IsIconic(hwnd) {
		winutil.ShowWindow(hwnd, 9) // SW_RESTORE
	}
	// Bring window to foreground
	winutil.SetForegroundWindow(hwnd)
}

func (u *UI) simpleLaunch(task string) {
	// Get task configuration
	var taskConfig *config.Task
	if task == u.cfg.CloudTask.ProcessName {
		taskConfig = &u.cfg.CloudTask
	} else if task == u.cfg.WaveFrontTask.ProcessName {
		taskConfig = &u.cfg.WaveFrontTask
	} else {
		log.Printf("Unknown task: %s", task)
		return
	}

	// Check for window by title hint if provided (works for any process type)
	titleHint := taskConfig.WindowTitleContains
	if strings.TrimSpace(titleHint) != "" {
		if hwnd := winutil.FindProcessWindowByExeAndTitleContains(task, titleHint); hwnd != 0 {
			log.Printf("simpleLaunch: found window by title contains '%s' for process %s", titleHint, task)
			bringWindowToFront(hwnd)
			return
		}
	}

	// Check if process is already running by process name (general case)
	_, hwnd, err := winutil.FindFirstWindowByProcessName(task)
	if err == nil && hwnd != 0 {
		log.Printf("simpleLaunch: found existing window for %s, bringing to front", task)
		bringWindowToFront(hwnd)
		return
	}

	// Handle system extensions (like .c3p files) using ShellExecute
	if taskConfig.IsSystemExtension {
		log.Printf("simpleLaunch: no existing process found, launching system extension %s", taskConfig.ProcessFilePath)
		// Determine working directory: prefer configured WorkingDirectory, else directory of the file
		workDir := strings.TrimSpace(taskConfig.WorkingDirectory)
		if workDir == "" && taskConfig.ProcessFilePath != "" {
			workDir = filepath.Dir(taskConfig.ProcessFilePath)
		}
		u.launchSystemExtension(taskConfig.ProcessFilePath, workDir)
		return
	}

	// Launch new process with arguments
	var cmd *exec.Cmd
	if len(taskConfig.Arguments) > 0 {
		cmd = exec.Command(taskConfig.ProcessFilePath, taskConfig.Arguments...)
	} else {
		cmd = exec.Command(taskConfig.ProcessFilePath)
	}
	// Set working directory: prefer configured WorkingDirectory, else directory of the executable
	workDir := strings.TrimSpace(taskConfig.WorkingDirectory)
	if workDir == "" && taskConfig.ProcessFilePath != "" {
		workDir = filepath.Dir(taskConfig.ProcessFilePath)
	}
	if workDir != "" {
		cmd.Dir = workDir
		log.Printf("simpleLaunch: setting working directory to '%s'", workDir)
	}

	err = cmd.Start()

	if err != nil {
		log.Printf("Failed to start %s: %v", task, err)
		return
	}

	log.Printf("Started %s with PID: %d", task, cmd.Process.Pid)

	// If we have a title hint, try to bring that specific window/tab to front shortly after launch
	if strings.TrimSpace(titleHint) != "" {
		hint := titleHint
		exe := task
		time.AfterFunc(1500*time.Millisecond, func() {
			if hwnd := winutil.FindProcessWindowByExeAndTitleContains(exe, hint); hwnd != 0 {
				log.Printf("post-launch: found window by title contains '%s' for process %s; bringing to front", hint, exe)
				bringWindowToFront(hwnd)
			} else {
				// Fallback: try any top-level window for the process
				if _, h, err := winutil.FindFirstWindowByProcessName(exe); err == nil && h != 0 {
					log.Printf("post-launch: fallback found top-level window for %s; bringing to front", exe)
					bringWindowToFront(h)
				}
			}
		})
	}
}

// launchSystemExtension uses ShellExecute to launch system extensions like .c3p files
func (u *UI) launchSystemExtension(filePath string, workDir string) {
	log.Printf("launchSystemExtension: attempting to launch %s (workDir='%s')", filePath, workDir)
	
	// Convert strings to UTF16 for Windows API
	filePathPtr, err := syscall.UTF16PtrFromString(filePath)
	if err != nil {
		log.Printf("launchSystemExtension: failed to convert file path to UTF16: %v", err)
		return
	}
	
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		log.Printf("launchSystemExtension: failed to convert verb to UTF16: %v", err)
		return
	}
	
	// Prepare lpDirectory if provided; if empty, try default to file's directory
	var dirPtr uintptr
	dir := strings.TrimSpace(workDir)
	if dir == "" {
		dir = filepath.Dir(filePath)
	}
	if dir != "" {
		dirUTF16, err := syscall.UTF16PtrFromString(dir)
		if err == nil {
			dirPtr = uintptr(unsafe.Pointer(dirUTF16))
		}
	}

	// Call ShellExecute to open the file with its associated application
	// ShellExecute(hwnd, lpOperation, lpFile, lpParameters, lpDirectory, nShowCmd)
	ret, _, _ := procShellExecute.Call(
		uintptr(u.hwnd),                               // hwnd - parent window handle
		uintptr(unsafe.Pointer(verbPtr)),              // lpOperation - "open"
		uintptr(unsafe.Pointer(filePathPtr)),          // lpFile - file to open
		0,                                            // lpParameters - no parameters
		dirPtr,                                       // lpDirectory - working directory
		SW_SHOWNORMAL,                                // nShowCmd - show normally
	)
	
	// ShellExecute returns a value > 32 on success
	if ret <= 32 {
		log.Printf("launchSystemExtension: ShellExecute failed with return code %d for file %s", ret, filePath)
		return
	}
	
	log.Printf("launchSystemExtension: successfully launched %s (return code: %d)", filePath, ret)
}
