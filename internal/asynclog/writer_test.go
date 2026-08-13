package asynclog

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type memoryWriteCloser struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	closed bool
}

func TestRotatingFileKeepsOnePreviousGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.log")
	dst, err := OpenRotatingFile(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "first\n" || string(current) != "second\n" {
		t.Fatalf("unexpected generations: previous=%q current=%q", previous, current)
	}
}

func (m *memoryWriteCloser) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buffer.Write(p)
}

func (m *memoryWriteCloser) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func TestWriterFlushesAndCloses(t *testing.T) {
	dst := &memoryWriteCloser{}
	w := New(dst, 4)
	if _, err := w.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dst.mu.Lock()
	defer dst.mu.Unlock()
	if got := dst.buffer.String(); got != "one\ntwo\n" {
		t.Fatalf("unexpected output %q", got)
	}
	if !dst.closed {
		t.Fatal("destination was not closed")
	}
}

func TestWriterFlushMakesQueuedRecordsVisible(t *testing.T) {
	dst := &memoryWriteCloser{}
	w := New(dst, 4)
	if _, err := w.Write([]byte("failure details\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(time.Second); err != nil {
		t.Fatal(err)
	}

	dst.mu.Lock()
	got := dst.buffer.String()
	dst.mu.Unlock()
	if got != "failure details\n" {
		t.Fatalf("unexpected flushed output %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWritesAndCloseDoNotPanic(t *testing.T) {
	dst := &memoryWriteCloser{}
	w := New(dst, 4)

	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				_, _ = w.Write([]byte("record\n"))
			}
		}()
	}
	_ = w.Close()
	writers.Wait()
}
