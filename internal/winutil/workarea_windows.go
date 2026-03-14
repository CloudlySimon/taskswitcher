package winutil

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type rect struct{ Left, Top, Right, Bottom int32 }

var (
	procSystemParametersInfo = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")
)

const SPI_GETWORKAREA = 0x0030

func GetWorkArea() (left, top, right, bottom int32, ok bool) {
	r := &rect{}
	r1, _, _ := procSystemParametersInfo.Call(uintptr(SPI_GETWORKAREA), 0, uintptr(unsafe.Pointer(r)), 0)
	if r1 == 0 {
		return 0, 0, 0, 0, false
	}
	return r.Left, r.Top, r.Right, r.Bottom, true
}
