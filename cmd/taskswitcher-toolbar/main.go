//go:build windows

package main

import (
	"crypto/sha256"
	"errors"
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

	"taskswitcher/internal/asynclog"
	"taskswitcher/internal/config"
	"taskswitcher/internal/winutil"

	"golang.org/x/sys/windows"
)

// Build-time visible version of the toolbar binary. Keep in sync with version.txt at repo root.
var AppVersion = "0.2.2-dev"

type UI struct {
	hwnd        windows.HWND
	cfg         *config.AppConfig
	configPath  string
	logPath     string
	mu          sync.Mutex
	buttonHWNDs map[int]windows.HWND
	buttonTasks map[int]configuredTask
	requests    chan taskRequest
	nextID      uint64
	workerState workerState
	windowCache map[string]winutil.HWND
	// Tracks the last time a WM_TIMER heartbeat was observed.
	lastTimerAt time.Time
}

type taskRequest struct {
	id          uint64
	key         string
	configIndex int
	task        config.Task
}

type configuredTask struct {
	index int
	task  config.Task
}

type workerState struct {
	requestID uint64
	taskKey   string
	startedAt time.Time
}

var (
	// Windows API functions
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")

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
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procPostMessage      = user32.NewProc("PostMessageW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procSetTimer         = user32.NewProc("SetTimer")
	procMessageBox       = user32.NewProc("MessageBoxW")
	procShellExecuteEx   = shell32.NewProc("ShellExecuteExW")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
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
	WM_DESTROY       = 0x0002
	WM_SYSKEYDOWN    = 0x0104
	SM_CXSCREEN      = 0
	WM_SETCURSOR     = 0x0020
	BN_CLICKED       = 0
	MB_OK            = 0
	MB_ICONERROR     = 0x00000010

	ID_TASK_BASE = 1001

	// ShellExecute constants
	SW_SHOWNORMAL            = 1
	SEE_MASK_NOCLOSEPROCESS  = 0x00000040
	SEE_MASK_NOASYNC         = 0x00000100
	SEE_MASK_FLAG_NO_UI      = 0x00000400
	SEE_MASK_UNICODE         = 0x00004000
	SEE_MASK_FLAG_LOG_USAGE  = 0x04000000
	COINIT_APARTMENTTHREADED = 0x2
	COINIT_DISABLE_OLE1DDE   = 0x4
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

type SHELLEXECUTEINFO struct {
	CbSize       uint32
	FMask        uint32
	Hwnd         windows.HWND
	LpVerb       *uint16
	LpFile       *uint16
	LpParameters *uint16
	LpDirectory  *uint16
	NShow        int32
	HInstApp     windows.Handle
	LpIDList     unsafe.Pointer
	LpClass      *uint16
	HkeyClass    windows.Handle
	DwHotKey     uint32
	HIcon        windows.Handle
	HProcess     windows.Handle
}

var globalUI *UI
var debugWriter *asynclog.Writer
var debugMode bool

// readVersionFile reads version.txt next to the executable. It intentionally
// avoids the working directory so kiosk startup is deterministic.
func readVersionFile() string {
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
	consoleWindow, _, _ := procGetConsoleWindow.Call()
	if consoleWindow != 0 {
		// Hide the console window
		procShowWindow.Call(consoleWindow, SW_HIDE)
	}
}

func showStartupError(message string) {
	title, _ := syscall.UTF16PtrFromString("TaskSwitcher startup error")
	body, _ := syscall.UTF16PtrFromString(message)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(title)), MB_OK|MB_ICONERROR)
}

func showTaskError(label string, failure error, configPath, logPath string, logWriteErr error) {
	if strings.TrimSpace(label) == "" {
		label = "task"
	}
	message := fmt.Sprintf("Could not open %s.\n\n%v\n\nConfiguration: %s\nDiagnostic log: %s", label, failure, configPath, logPath)
	if logWriteErr != nil {
		message += fmt.Sprintf("\n\nWarning: the diagnostic log could not be flushed: %v", logWriteErr)
	}
	title, _ := syscall.UTF16PtrFromString("TaskSwitcher launch failed")
	body, _ := syscall.UTF16PtrFromString(message)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(title)), MB_OK|MB_ICONERROR)
}

func writeVersionOutput() {
	line := []byte(AppVersion + "\r\n")
	if err := writeStdout(line); err == nil {
		return
	}

	// GUI-subsystem binaries normally have no console. Attach to the invoking
	// shell and reacquire its stdout handle so `-version` remains scriptable.
	attached, _, _ := procAttachConsole.Call(uintptr(0xFFFFFFFF)) // ATTACH_PARENT_PROCESS
	if attached != 0 {
		if err := writeStdout(line); err == nil {
			return
		}
	}

	// This only applies when -version was launched without a console or output
	// redirection, for example by double-clicking a shortcut.
	showStartupError("TaskSwitcher version " + AppVersion)
}

func writeStdout(contents []byte) error {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return errors.New("standard output is unavailable")
	}
	var written uint32
	if err := windows.WriteFile(handle, contents, &written, nil); err != nil {
		return err
	}
	if int(written) != len(contents) {
		return io.ErrShortWrite
	}
	return nil
}

func executableDirectory() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func fileSHA256(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	digest := sha256.Sum256(contents)
	return fmt.Sprintf("%x", digest)
}

func operationalLogPaths() []string {
	paths := []string{filepath.Join(executableDirectory(), "taskswitcher-toolbar.log")}
	if cacheDir, err := os.UserCacheDir(); err == nil && cacheDir != "" {
		fallback := filepath.Join(cacheDir, "TaskSwitcher", "taskswitcher-toolbar.log")
		if fallback != paths[0] {
			paths = append(paths, fallback)
		}
	}
	return paths
}

func openOperationalLog() (io.WriteCloser, string, error) {
	var failures []string
	for _, path := range operationalLogPaths() {
		file, err := asynclog.OpenRotatingFile(path, 5*1024*1024)
		if err == nil {
			return file, path, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", path, err))
	}
	return nil, "", fmt.Errorf("could not create the TaskSwitcher log file (%s)", strings.Join(failures, "; "))
}

func main() {
	// Flags
	var configPath string
	var debugEnabled bool
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "Path to config file (defaults to config.json next to the executable)")
	flag.BoolVar(&debugEnabled, "debug", false, "Enable verbose operational logging")
	flag.BoolVar(&showVersion, "version", false, "Print the executable version and exit")
	flag.Parse()
	if showVersion {
		writeVersionOutput()
		return
	}
	debugMode = debugEnabled
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(executableDirectory(), "config.json")
	} else if abs, err := filepath.Abs(configPath); err == nil {
		configPath = abs
	}

	releaseInstance, acquired, err := winutil.AcquireSingleInstance("Local\\TaskSwitcherToolbar")
	if err != nil {
		showStartupError(fmt.Sprintf("Failed to create single-instance mutex: %v", err))
		return
	}
	if !acquired {
		return
	}
	defer releaseInstance()

	// Always retain a bounded operational log. Prefer a discoverable file next
	// to the executable and fall back to the current user's cache directory.
	// The async writer ensures that slow storage cannot block the UI thread.
	logFile, logPath, err := openOperationalLog()
	if err != nil {
		showStartupError(err.Error())
		return
	}
	debugWriter = asynclog.New(logFile, 1024)
	log.SetOutput(debugWriter)
	defer func() {
		if debugWriter != nil {
			if dropped := debugWriter.Dropped(); dropped != 0 {
				log.Printf("async logger dropped %d records", dropped)
			}
			_ = debugWriter.Close()
		}
	}()

	if debugEnabled {
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
	}
	executablePath, _ := os.Executable()
	log.Printf("TaskSwitcher starting: version=%s executable=%q config=%q configSHA256=%s log=%q debug=%v", AppVersion, executablePath, configPath, fileSHA256(configPath), logPath, debugEnabled)

	cfg, configCreated, err := config.LoadOrCreate(configPath)
	if err != nil {
		message := fmt.Sprintf("Failed to load config %q: %v\n\nDiagnostic log: %s", configPath, err, logPath)
		log.Printf("%s", message)
		_ = debugWriter.Flush(2 * time.Second)
		showStartupError(message)
		return
	}
	if configCreated {
		log.Printf("Default config created: %q", configPath)
		log.Printf("TaskSwitcher is exiting after first-run config generation; review tasks[].processName, tasks[].processFilePath, and tasks[].windowTitleContains, then start it again")
		_ = debugWriter.Flush(2 * time.Second)
		return
	}

	log.Printf("Config loaded successfully: tasks=%d", len(cfg.Tasks))
	for index, task := range cfg.Tasks {
		log.Printf("  tasks[%d]: id=%q processName=%q buttonLabel=%q enabled=%v", index, task.ID, task.ProcessName, task.ButtonLabel, task.IsEnabled())
	}

	// Hide console window
	hideConsoleWindow()

	log.Printf("Creating UI instance...")
	ui := &UI{
		cfg:         cfg,
		configPath:  configPath,
		logPath:     logPath,
		buttonHWNDs: make(map[int]windows.HWND),
		buttonTasks: make(map[int]configuredTask),
		requests:    make(chan taskRequest, 1),
		windowCache: make(map[string]winutil.HWND),
	}
	globalUI = ui // Set global reference for window procedure
	log.Printf("Global UI reference set")
	log.Printf("Starting UI run...")
	if err := ui.run(); err != nil {
		log.Printf("Toolbar stopped with an error: %v", err)
		showStartupError(err.Error())
	}
}

func (u *UI) run() error {
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
	atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return fmt.Errorf("register window class: %w", callErr)
	}
	log.Printf("Step 5 COMPLETE: Window class registered successfully, atom=%d", atom)

	// Get work area dimensions
	log.Printf("Step 6: Getting work area dimensions...")
	left, top, right, bottom, ok := winutil.GetWorkArea()
	if !ok {
		return errors.New("failed to get Windows work area")
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
	hwnd, _, callErr := procCreateWindowEx.Call(
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
		return fmt.Errorf("create toolbar window: %w", callErr)
	}
	log.Printf("Step 8 COMPLETE: Main window created successfully, hwnd=%d", hwnd)

	u.hwnd = windows.HWND(hwnd)

	// Create buttons
	log.Printf("Step 9: Creating buttons...")
	u.createButtons()
	log.Printf("Step 9 COMPLETE: Buttons created")
	go u.commandWorker()

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

	// Observe UI message-pump latency from outside the UI thread. This monitor is
	// diagnostic only; it never mutates command state or posts recovery messages.
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		var lastReported time.Time
		for range ticker.C {
			u.mu.Lock()
			lastT := u.lastTimerAt
			worker := u.workerState
			u.mu.Unlock()

			if !lastT.IsZero() {
				gap := time.Since(lastT)
				if gap > 2500*time.Millisecond && (lastReported.IsZero() || time.Since(lastReported) >= 10*time.Second) {
					log.Printf("UI heartbeat delayed by %v; worker request=%d task=%q age=%v", gap, worker.requestID, worker.taskKey, workerAge(worker))
					lastReported = time.Now()
				}
			}
		}
	}()

	// Start message loop (blocking)
	log.Printf("Step 11: Starting message loop (BLOCKING)...")
	log.Printf("=== APPLICATION STARTUP COMPLETE ===")
	if err := u.messageLoop(); err != nil {
		return err
	}
	log.Printf("Step 11 COMPLETE: Message loop ended - application shutting down")
	return nil
}

func (u *UI) messageLoop() error {
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
			return errors.New("GetMessageW failed")
		}
		if ret == 0 { // WM_QUIT
			log.Printf("WM_QUIT received, exiting message loop")
			break
		}

		msgCount++
		if debugMode && ((msgCount%100 == 1 && msg.Message != WM_SETCURSOR) || msg.Message == WM_COMMAND || msg.Message == WM_DESTROY) {
			log.Printf("Processing message %d: type=%d, wParam=%d, lParam=%d", msgCount, msg.Message, msg.WParam, msg.LParam)
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	log.Printf("=== MESSAGE LOOP ENDED ===")
	return nil
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
		if debugMode {
			log.Printf("WM_COMMAND received: id=%d, notify=%d, childHWND=%d", cmdID, notifyCode, lParam)
		}

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
		configured, ok := globalUI.buttonTasks[cmdID]
		if !ok {
			return 0
		}
		globalUI.requestTask(configured.index, configured.task)
		return 0
	case WM_TIMER:
		if globalUI != nil {
			globalUI.mu.Lock()
			globalUI.lastTimerAt = time.Now()
			globalUI.mu.Unlock()
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
		if debugMode && msg != 15 && msg != 20 && msg != 132 && msg != 30 && msg != 512 {
			log.Printf("Window message: %d (wParam=%d, lParam=%d)", msg, wParam, lParam)
		}
	}
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

	for index, task := range u.cfg.Tasks {
		if !task.IsEnabled() {
			continue
		}
		commandID := ID_TASK_BASE + index
		buttons = append(buttons, buttonInfo{text: task.ButtonLabel, width: 220, id: commandID})
		u.buttonTasks[commandID] = configuredTask{index: index, task: task}
	}
	if len(buttons) == 0 {
		return
	}
	availableWidth := int(screenWidth) - buttonSpacing*(len(buttons)-1)
	if fittedWidth := availableWidth / len(buttons); fittedWidth < 220 {
		for index := range buttons {
			buttons[index].width = fittedWidth
		}
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
	text  string
	width int
	id    int
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

func (u *UI) requestTask(configIndex int, task config.Task) {
	key := task.ID
	u.mu.Lock()
	u.nextID++
	request := taskRequest{id: u.nextID, key: key, configIndex: configIndex, task: task}
	u.mu.Unlock()

	select {
	case u.requests <- request:
		log.Printf("request %d queued for %s", request.id, key)
		return
	default:
	}

	var replaced taskRequest
	select {
	case replaced = <-u.requests:
	default:
	}
	select {
	case u.requests <- request:
		log.Printf("request %d for %s replaced queued request %d for %s", request.id, key, replaced.id, replaced.key)
	default:
		// The worker won the race and consumed the queued request. Dropping this
		// extra tap is preferable to ever blocking the UI thread.
		log.Printf("request %d for %s dropped during queue handoff", request.id, key)
	}
}

func workerAge(state workerState) time.Duration {
	if state.startedAt.IsZero() {
		return 0
	}
	return time.Since(state.startedAt)
}

func (u *UI) commandWorker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hresult, _, _ := procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED|COINIT_DISABLE_OLE1DDE)
	comInitialized := int32(hresult) >= 0
	if comInitialized {
		defer procCoUninitialize.Call()
	} else {
		log.Printf("CoInitializeEx failed: HRESULT=%#x; executable launches remain available", uint32(hresult))
	}

	for request := range u.requests {
		started := time.Now()
		u.mu.Lock()
		u.workerState = workerState{requestID: request.id, taskKey: request.key, startedAt: started}
		u.mu.Unlock()

		err := u.executeRequest(request)
		if err != nil {
			u.logTaskFailure(request, time.Since(started), err)
			var flushErr error
			if debugWriter != nil {
				flushErr = debugWriter.Flush(2 * time.Second)
			}
			showTaskError(request.task.ButtonLabel, err, u.configPath, u.logPath, flushErr)
		} else {
			log.Printf("request %d for %s completed in %v", request.id, request.key, time.Since(started))
		}

		u.mu.Lock()
		if u.workerState.requestID == request.id {
			u.workerState = workerState{}
		}
		u.mu.Unlock()
	}
}

func (u *UI) logTaskFailure(request taskRequest, elapsed time.Duration, failure error) {
	section := fmt.Sprintf("tasks[%d]", request.configIndex)
	effectiveWorkingDirectory := strings.TrimSpace(request.task.WorkingDirectory)
	if effectiveWorkingDirectory == "" && request.task.ProcessFilePath != "" {
		effectiveWorkingDirectory = filepath.Dir(request.task.ProcessFilePath)
	}
	log.Printf("TASK LAUNCH FAILED\n  requestID: %d\n  elapsed: %v\n  button: %q\n  configFile: %q\n  logFile: %q\n  failure: %v\n  relevant config values:\n    %s.id: %q\n    %s.processName: %q\n    %s.processFilePath: %q\n    %s.arguments: %q\n    %s.workingDirectory: %q\n    effectiveWorkingDirectory: %q\n    %s.windowTitleContains: %q\n    %s.switchOnly: %v\n    %s.isSystemExtension: %v",
		request.id,
		elapsed,
		request.task.ButtonLabel,
		u.configPath,
		u.logPath,
		failure,
		section, request.task.ID,
		section, request.task.ProcessName,
		section, request.task.ProcessFilePath,
		section, request.task.Arguments,
		section, request.task.WorkingDirectory,
		effectiveWorkingDirectory,
		section, request.task.WindowTitleContains,
		section, request.task.IsSwitchOnly(),
		section, request.task.IsSystemExtension,
	)
}

func (u *UI) executeRequest(request taskRequest) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic while handling %s: %v", request.key, recovered)
		}
	}()
	return u.switchOrLaunch(request.configIndex, request.key, request.task)
}

func bringWindowToFront(hwnd winutil.HWND) error {
	if !winutil.IsWindow(hwnd) {
		return errors.New("target window is no longer valid")
	}
	if winutil.IsIconic(hwnd) {
		winutil.ShowWindow(hwnd, winutil.SW_RESTORE)
	}
	if !winutil.SetForegroundWindow(hwnd) {
		return errors.New("Windows rejected foreground activation")
	}
	return nil
}

func (u *UI) switchOrLaunch(configIndex int, key string, task config.Task) error {
	section := fmt.Sprintf("tasks[%d]", configIndex)
	if cached := u.windowCache[key]; winutil.IsWindow(cached) {
		if err := bringWindowToFront(cached); err == nil {
			return nil
		}
		delete(u.windowCache, key)
	}

	lookupStarted := time.Now()
	_, hwnd, lookupErr := winutil.FindProcessWindow(task.ProcessName, task.WindowTitleContains)
	log.Printf("window lookup for %s completed in %v (found=%v, err=%v)", key, time.Since(lookupStarted), hwnd != 0, lookupErr)
	if hwnd != 0 {
		u.windowCache[key] = hwnd
		return bringWindowToFront(hwnd)
	}
	if task.IsSwitchOnly() {
		return fmt.Errorf("no visible window matched %s.processName=%q and %s.windowTitleContains=%q; %s.switchOnly is true, so TaskSwitcher will not launch it. Correct the process name/title, start the application separately, or set switchOnly to false and configure processFilePath", section, task.ProcessName, section, task.WindowTitleContains, section)
	}

	workDir := strings.TrimSpace(task.WorkingDirectory)
	if workDir == "" && task.ProcessFilePath != "" {
		workDir = filepath.Dir(task.ProcessFilePath)
	}
	if err := validateLaunchTarget(section, task, workDir); err != nil {
		return err
	}

	if task.IsSystemExtension {
		if err := launchSystemExtension(task.ProcessFilePath, workDir); err != nil {
			extension := filepath.Ext(task.ProcessFilePath)
			return fmt.Errorf("Windows could not open %s.processFilePath=%q: %w. Confirm that the file exists and that Windows has an application associated with %q files", section, task.ProcessFilePath, err, extension)
		}
	} else {
		cmd := exec.Command(task.ProcessFilePath, task.Arguments...)
		cmd.Dir = workDir
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("Windows could not start %s.processFilePath=%q: %w. Check the executable path, %s.arguments, file permissions, and %s.workingDirectory", section, task.ProcessFilePath, err, section, section)
		}
		pid := cmd.Process.Pid
		if err := cmd.Process.Release(); err != nil {
			log.Printf("warning: %s started with PID %d but its process handle could not be released: %v", key, pid, err)
		}
		log.Printf("started %s with PID %d", key, pid)
	}

	// Keep post-launch discovery serialized with other commands. The UI remains
	// responsive, while no detached timer can race a later request.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		_, hwnd, err := winutil.FindProcessWindow(task.ProcessName, task.WindowTitleContains)
		if err == nil && hwnd != 0 {
			u.windowCache[key] = hwnd
			return bringWindowToFront(hwnd)
		}
	}
	return fmt.Errorf("the launch request returned successfully, but after 5 seconds no visible window matched %s.processName=%q and %s.windowTitleContains=%q. If the application opened, correct those matching fields; otherwise check %s.processFilePath and %s.arguments", section, task.ProcessName, section, task.WindowTitleContains, section, section)
}

func validateLaunchTarget(section string, task config.Task, workDir string) error {
	path := strings.TrimSpace(task.ProcessFilePath)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("config field %s.processFilePath=%q is relative. Use the full Windows path, for example C:\\Program Files\\Vendor\\application.exe", section, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config field %s.processFilePath=%q does not exist. Correct the path in config.json or install/copy the target file there", section, path)
		}
		return fmt.Errorf("config field %s.processFilePath=%q cannot be accessed: %w. Check the path and the kiosk account's permissions", section, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config field %s.processFilePath=%q points to a directory; it must point to an executable or associated file", section, path)
	}
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	if !filepath.IsAbs(workDir) {
		return fmt.Errorf("%s.workingDirectory=%q is relative. Use a full Windows directory path or remove the field to use the processFilePath directory", section, workDir)
	}
	workInfo, err := os.Stat(workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the working directory %q does not exist. Correct %s.workingDirectory, or remove it to use the directory containing processFilePath", workDir, section)
		}
		return fmt.Errorf("the working directory %q cannot be accessed: %w. Check %s.workingDirectory and permissions", workDir, err, section)
	}
	if !workInfo.IsDir() {
		return fmt.Errorf("%s.workingDirectory resolves to %q, which is not a directory", section, workDir)
	}
	return nil
}

func launchSystemExtension(filePath string, workDir string) error {
	filePathPtr, err := syscall.UTF16PtrFromString(filePath)
	if err != nil {
		return fmt.Errorf("encode associated file path: %w", err)
	}
	verbPtr, _ := syscall.UTF16PtrFromString("open")
	var dirPtr *uint16
	if strings.TrimSpace(workDir) != "" {
		dirPtr, err = syscall.UTF16PtrFromString(workDir)
		if err != nil {
			return fmt.Errorf("encode working directory: %w", err)
		}
	}

	info := SHELLEXECUTEINFO{
		CbSize:      uint32(unsafe.Sizeof(SHELLEXECUTEINFO{})),
		FMask:       SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NOASYNC | SEE_MASK_FLAG_NO_UI | SEE_MASK_UNICODE | SEE_MASK_FLAG_LOG_USAGE,
		LpVerb:      verbPtr,
		LpFile:      filePathPtr,
		LpDirectory: dirPtr,
		NShow:       SW_SHOWNORMAL,
	}
	started := time.Now()
	ok, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return fmt.Errorf("ShellExecuteExW %s after %v: %w", filePath, time.Since(started), callErr)
	}
	if info.HProcess != 0 {
		_ = windows.CloseHandle(info.HProcess)
	}
	log.Printf("associated-file launch for %s returned in %v", filePath, time.Since(started))
	return nil
}
