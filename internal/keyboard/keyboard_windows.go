package keyboard

import (
	"os/exec"
	"sync"
	"time"

	"taskswitcher/internal/winutil"
)

type Keyboard struct {
	mu     sync.Mutex
	hwnd   winutil.HWND
	hidden bool
	t      *time.Ticker
}

func New() *Keyboard { return &Keyboard{} }

const oskName = "osk"

func (k *Keyboard) Show(x, y int, height, width int) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.hidden = false
	_, hwnd, err := winutil.FindFirstWindowByProcessName(oskName)
	if err != nil || hwnd == 0 {
		cmd := exec.Command(oskName + ".exe")
		if err := cmd.Start(); err != nil {
			return err
		}
	}
	k.hwnd = hwnd
	k.t = time.NewTicker(1 * time.Second)
	go k.tick(x, y-height, width, height)
	return nil
}

func (k *Keyboard) Hide() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.hidden = true
	if k.t != nil {
		k.t.Stop()
	}
	if k.hwnd != 0 {
		winutil.SetWindowPos(k.hwnd, 0, 0, 0, 0, 0, winutil.SWP_HIDEWINDOW)
	}
	return nil
}

func (k *Keyboard) Toggle(x, y int, height, width int) error {
	if k.hidden {
		return k.Show(x, y, height, width)
	}
	return k.Hide()
}

func (k *Keyboard) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.t != nil {
		k.t.Stop()
	}
}

func (k *Keyboard) tick(x, y, w, h int) {
	for range k.t.C {
		k.mu.Lock()
		if k.hidden {
			k.mu.Unlock()
			return
		}
		if k.hwnd != 0 {
			winutil.SetWindowPos(k.hwnd, 0, x, y, w, h, winutil.SWP_SHOWWINDOW)
		}
		k.mu.Unlock()
	}
}
