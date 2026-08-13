package winutil

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procCreateMutexW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")

// AcquireSingleInstance acquires a named, per-user-session mutex. The caller
// must call the returned release function when acquired is true.
func AcquireSingleInstance(name string) (release func(), acquired bool, err error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return nil, false, fmt.Errorf("CreateMutexW: %w", callErr)
	}
	handle := windows.Handle(h)
	if callErr == windows.ERROR_ALREADY_EXISTS {
		_ = windows.CloseHandle(handle)
		return nil, false, nil
	}
	return func() { _ = windows.CloseHandle(handle) }, true, nil
}
