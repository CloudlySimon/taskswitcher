package winutil

import (
	"errors"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type HWND uintptr

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
)

const (
	SW_RESTORE = 9
	HWND_TOPMOST = ^uintptr(0)
	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOACTIVATE = 0x0010
	SWP_SHOWWINDOW = 0x0040
	SWP_HIDEWINDOW = 0x0080
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

func SetWindowPos(hwnd HWND, hWndInsertAfter uintptr, x, y, cx, cy int, flags uint32) bool {
	r, _, _ := procSetWindowPos.Call(uintptr(hwnd), hWndInsertAfter, uintptr(x), uintptr(y), uintptr(cx), uintptr(cy), uintptr(flags))
	return r != 0
}

func FindFirstWindowByProcessName(name string) (uint32, HWND, error) {
	pid, err := findFirstPidByName(name)
	if err != nil {
		return 0, 0, err
	}
	hwnd := findTopLevelWindowByPID(pid)
	return pid, hwnd, nil
}

func findFirstPidByName(name string) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)

	pe := new(windows.ProcessEntry32)
	pe.Size = uint32(unsafe.Sizeof(*pe))
	if err = windows.Process32First(snap, pe); err != nil {
		return 0, err
	}
	for {
		exe := windows.UTF16ToString(pe.ExeFile[:])
		if exe == name || exe == name+".exe" {
			return pe.ProcessID, nil
		}
		if err = windows.Process32Next(snap, pe); err != nil {
			break
		}
	}
	return 0, errors.New("process not found")
}

func findTopLevelWindowByPID(pid uint32) HWND {
	var found HWND
	cb := windows.NewCallback(func(hwnd HWND, lparam uintptr) uintptr {
		var winpid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&winpid)))
		if winpid == pid {
			// Check if window is visible and not a child window
			isVisible, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
			if isVisible != 0 {
				// Check if it's a top-level window (has no parent)
				procGetParent := user32.NewProc("GetParent")
				parent, _, _ := procGetParent.Call(uintptr(hwnd))
				if parent == 0 {
					found = hwnd
					return 0 // Stop enumeration
				}
			}
		}
		return 1 // Continue enumeration
	})
	procEnumWindows.Call(cb, 0)
	return found
}

func getWindowText(hwnd HWND) string {
	// Get length in characters (excluding null terminator)
	length, _, _ := procGetWindowTextLengthW.Call(uintptr(hwnd))
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func getExeNameByPid(target uint32) string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(snap)

	pe := new(windows.ProcessEntry32)
	pe.Size = uint32(unsafe.Sizeof(*pe))
	if err = windows.Process32First(snap, pe); err != nil {
		return ""
	}
	for {
		if pe.ProcessID == target {
			exe := windows.UTF16ToString(pe.ExeFile[:])
			return exe
		}
		if err = windows.Process32Next(snap, pe); err != nil {
			break
		}
	}
	return ""
}

func FindProcessWindowByExeAndTitleContains(exeName string, titleSub string) HWND {
	if exeName == "" || titleSub == "" {
		return 0
	}
	exeNameLower := strings.ToLower(exeName)
	subLower := strings.ToLower(titleSub)
	var found HWND
	cb := windows.NewCallback(func(hwnd HWND, lparam uintptr) uintptr {
		// Visible and top-level only
		isVisible, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
		if isVisible == 0 {
			return 1
		}
		procGetParent := user32.NewProc("GetParent")
		parent, _, _ := procGetParent.Call(uintptr(hwnd))
		if parent != 0 {
			return 1
		}
		var winpid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&winpid)))
		exe := strings.ToLower(getExeNameByPid(winpid))
		if exe == exeNameLower || exe == exeNameLower+".exe" {
			title := strings.ToLower(getWindowText(hwnd))
			if title != "" && strings.Contains(title, subLower) {
				found = hwnd
				return 0 // stop
			}
		}
		return 1 // continue
	})
	procEnumWindows.Call(cb, 0)
	return found
}

func RunShutdown(args string) error {
	cmd := exec.Command("shutdown", args)
	return cmd.Start()
}

