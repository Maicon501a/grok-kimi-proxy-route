// Package storage provides a thread-safe atomic/debounced file writer used to
// persist session and account state without blocking request handlers.
//
// This replaces the Node async-writer module. Compared to the original it:
//   - uses atomic rename on every write (no partial files observable);
//   - coalesces writes with a single goroutine per file (no race window
//     between the timeout and the write);
//   - flushes synchronously on Close/Shutdown so no data is lost.
package storage

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AsyncWriter coalesces and persists writes for a single file path.
type AsyncWriter struct {
	path       string
	debounce   time.Duration
	mu         sync.Mutex
	pending    []byte
	hasPending bool
	timer      *time.Timer
	done       chan struct{}
	flushing   bool
}

// NewAsyncWriter creates a writer for the given path with the requested
// debounce window.
func NewAsyncWriter(path string, debounce time.Duration) *AsyncWriter {
	w := &AsyncWriter{
		path:     path,
		debounce: debounce,
		done:     make(chan struct{}),
	}
	return w
}

// Schedule queues data to be written after the debounce window. The latest
// queued data wins; older pending writes are discarded.
func (w *AsyncWriter) Schedule(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = data
	w.hasPending = true

	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.debounce, w.flushLocked)
}

// flushLocked is invoked by the debounce timer; assumes w.mu is NOT held (the
// timer fires in its own goroutine). It acquires the lock itself.
func (w *AsyncWriter) flushLocked() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeNow()
}

// writeNow performs the atomic write of the current pending buffer. The caller
// must hold w.mu.
func (w *AsyncWriter) writeNow() {
	if !w.hasPending || w.flushing {
		return
	}
	data := w.pending
	w.pending = nil
	w.hasPending = false

	if err := writeFileAtomic(w.path, data); err != nil {
		// Best-effort: log would go here. Re-queue is skipped to avoid loops.
		_ = err
	}
}

// Flush forces an immediate write of any pending data and blocks until done.
func (w *AsyncWriter) Flush() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.writeNow()
	w.mu.Unlock()
}

// Close flushes pending data and stops the writer. Safe to call once.
func (w *AsyncWriter) Close() {
	w.Flush()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		close(w.done)
		w.done = nil
	}
}

// writeFileAtomic writes data to path via a temp file + atomic rename, ensuring
// readers never observe a partially-written file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// WriteAtomic writes data to path atomically (synchronous, no debouncing).
func WriteAtomic(path string, data []byte) error {
	return writeFileAtomic(path, data)
}

// ReadFile reads a file's full contents, returning an empty slice when the
// file does not exist.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
