package asynclog

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type rotatingFile struct {
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// OpenRotatingFile opens path for append and keeps one previous generation at
// path + ".1". Rotation happens before a write that would exceed maxBytes.
func OpenRotatingFile(path string, maxBytes int64) (io.WriteCloser, error) {
	if maxBytes < 1 {
		maxBytes = 5 * 1024 * 1024
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFile{path: path, maxBytes: maxBytes, file: f, size: info.Size()}, nil
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	if f.size > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) Close() error { return f.file.Close() }

func (f *rotatingFile) rotate() error {
	if err := f.file.Close(); err != nil {
		return err
	}
	_ = os.Remove(f.path + ".1")
	if err := os.Rename(f.path, f.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	next, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	f.file = next
	f.size = 0
	return nil
}

// Writer decouples callers from a potentially slow destination. Writes are
// copied into a bounded queue; when the queue is full, records are dropped
// rather than blocking a UI or worker thread.
type Writer struct {
	dst           io.WriteCloser
	queue         chan []byte
	flushRequests chan chan error
	done          chan struct{}
	closed        atomic.Bool
	dropped       atomic.Uint64
	once          sync.Once
	sendMu        sync.RWMutex
}

func New(dst io.WriteCloser, capacity int) *Writer {
	if capacity < 1 {
		capacity = 1
	}
	w := &Writer{
		dst:           dst,
		queue:         make(chan []byte, capacity),
		flushRequests: make(chan chan error),
		done:          make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *Writer) Write(p []byte) (int, error) {
	w.sendMu.RLock()
	defer w.sendMu.RUnlock()
	if w.closed.Load() {
		return len(p), nil
	}
	record := append([]byte(nil), p...)
	select {
	case w.queue <- record:
	default:
		// Preserve the newest operational evidence. When full, discard one old
		// record and make a best-effort non-blocking enqueue of the new one.
		droppedOld := false
		select {
		case <-w.queue:
			droppedOld = true
		default:
		}
		if droppedOld {
			w.dropped.Add(1)
		}
		select {
		case w.queue <- record:
		default:
			w.dropped.Add(1)
		}
	}
	return len(p), nil
}

func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

// Flush waits up to timeout for all records queued before and during the flush
// request to reach the destination. It is intended for important failure
// records that an operator may inspect immediately.
func (w *Writer) Flush(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	w.sendMu.RLock()
	if w.closed.Load() {
		w.sendMu.RUnlock()
		return nil
	}
	result := make(chan error, 1)
	select {
	case w.flushRequests <- result:
	case <-deadline.C:
		w.sendMu.RUnlock()
		return os.ErrDeadlineExceeded
	}

	select {
	case err := <-result:
		w.sendMu.RUnlock()
		return err
	case <-deadline.C:
		w.sendMu.RUnlock()
		return os.ErrDeadlineExceeded
	}
}

func (w *Writer) Close() error {
	var err error
	w.once.Do(func() {
		w.sendMu.Lock()
		w.closed.Store(true)
		close(w.queue)
		w.sendMu.Unlock()
		<-w.done
		err = w.dst.Close()
	})
	return err
}

func (w *Writer) run() {
	defer close(w.done)
	buf := bufio.NewWriterSize(w.dst, 64*1024)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer buf.Flush()

	for {
		select {
		case record, ok := <-w.queue:
			if !ok {
				return
			}
			_, _ = buf.Write(record)
		case result := <-w.flushRequests:
			draining := true
			for draining {
				select {
				case record, ok := <-w.queue:
					if !ok {
						draining = false
						continue
					}
					_, _ = buf.Write(record)
				default:
					draining = false
				}
			}
			result <- buf.Flush()
		case <-ticker.C:
			_ = buf.Flush()
		}
	}
}
