package winutil

import (
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

type HWND uintptr

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindow                 = user32.NewProc("IsWindow")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetParent                = user32.NewProc("GetParent")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")

	enumWindowsCallback = windows.NewCallback(enumWindowsProc)
	windowSearches      sync.Map
	nextWindowSearchID  atomic.Uint64
)

const (
	SW_RESTORE     = 9
	HWND_TOPMOST   = ^uintptr(0)
	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOACTIVATE = 0x0010
	SWP_SHOWWINDOW = 0x0040
	SWP_HIDEWINDOW = 0x0080

	maxWindowTitleChars = 4096
)

func SetForegroundWindow(hwnd HWND) bool {
	r, _, _ := procSetForegroundWindow.Call(uintptr(hwnd))
	return r != 0
}

func ShowWindow(hwnd HWND, nCmdShow int) bool {
	r, _, _ := procShowWindow.Call(uintptr(hwnd), uintptr(nCmdShow))
	return r != 0
}

func IsIconic(hwnd HWND) bool {
	r, _, _ := procIsIconic.Call(uintptr(hwnd))
	return r != 0
}

func IsWindow(hwnd HWND) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(uintptr(hwnd))
	return r != 0
}

func SetWindowPos(hwnd HWND, hWndInsertAfter uintptr, x, y, cx, cy int, flags uint32) bool {
	r, _, _ := procSetWindowPos.Call(uintptr(hwnd), hWndInsertAfter, uintptr(x), uintptr(y), uintptr(cx), uintptr(cy), uintptr(flags))
	return r != 0
}

// FindFirstWindowByProcessName returns the first eligible top-level window
// owned by any process whose executable name matches name.
func FindFirstWindowByProcessName(name string) (uint32, HWND, error) {
	return FindProcessWindow(name, "")
}

// FindProcessWindow locates an eligible top-level window by executable name.
// A supplied case-insensitive title substring is preferred, with any eligible
// window for the process as a fallback. It takes exactly one process snapshot
// and performs one window enumeration.
func FindProcessWindow(exeName, titleContains string) (uint32, HWND, error) {
	normalizedName := normalizeExeName(exeName)
	if normalizedName == "" {
		return 0, 0, errors.New("process name is empty")
	}

	pids, err := matchingProcessIDs(normalizedName)
	if err != nil {
		return 0, 0, err
	}
	if len(pids) == 0 {
		return 0, 0, errors.New("process not found")
	}

	state := &windowSearchState{
		pids:          pids,
		titleContains: strings.ToLower(strings.TrimSpace(titleContains)),
	}
	searchID := nextWindowSearchID.Add(1)
	windowSearches.Store(searchID, state)
	defer windowSearches.Delete(searchID)
	procEnumWindows.Call(enumWindowsCallback, uintptr(searchID))
	if state.hwnd == 0 {
		state.pid = state.fallbackPID
		state.hwnd = state.fallbackHWND
	}
	if state.hwnd == 0 {
		return 0, 0, errors.New("process window not found")
	}
	return state.pid, state.hwnd, nil
}

func FindProcessWindowByExeAndTitleContains(exeName string, titleSub string) HWND {
	_, hwnd, _ := FindProcessWindow(exeName, titleSub)
	return hwnd
}

type windowSearchState struct {
	pids          map[uint32]struct{}
	titleContains string
	pid           uint32
	hwnd          HWND
	fallbackPID   uint32
	fallbackHWND  HWND
}

func enumWindowsProc(hwnd HWND, lparam uintptr) uintptr {
	value, ok := windowSearches.Load(uint64(lparam))
	if !ok {
		return 0
	}
	state, ok := value.(*windowSearchState)
	if !ok || state == nil {
		return 0
	}

	visible, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	if visible == 0 {
		return 1
	}
	parent, _, _ := procGetParent.Call(uintptr(hwnd))
	if parent != 0 {
		return 1
	}

	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if _, ok := state.pids[pid]; !ok {
		return 1
	}
	if state.fallbackHWND == 0 {
		state.fallbackPID = pid
		state.fallbackHWND = hwnd
	}
	if state.titleContains != "" {
		title := strings.ToLower(getWindowText(hwnd))
		if title == "" || !strings.Contains(title, state.titleContains) {
			return 1
		}
	}

	state.pid = pid
	state.hwnd = hwnd
	return 0
}

func matchingProcessIDs(normalizedName string) (map[uint32]struct{}, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	pids := make(map[uint32]struct{})
	pe := new(windows.ProcessEntry32)
	pe.Size = uint32(unsafe.Sizeof(*pe))
	if err = windows.Process32First(snap, pe); err != nil {
		return nil, err
	}
	for {
		if normalizeExeName(windows.UTF16ToString(pe.ExeFile[:])) == normalizedName {
			pids[pe.ProcessID] = struct{}{}
		}
		if err = windows.Process32Next(snap, pe); err != nil {
			break
		}
	}
	return pids, nil
}

func getWindowText(hwnd HWND) string {
	// Top-level cross-process captions are returned from USER32 without
	// synchronously messaging the owning process. A fixed bound avoids a
	// separate length call and is ample for a window title.
	buf := make([]uint16, maxWindowTitleChars)
	length, _, _ := procGetWindowTextW.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if length == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:length])
}

func RunShutdown(args string) error {
	cmd := exec.Command("shutdown", args)
	return cmd.Start()
}
