package logging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultQueueSize   = 4096
	defaultWriteBuffer = 64 * 1024
	flushInterval      = 250 * time.Millisecond
)

// AsyncFileWriter is an asynchronous file sink with buffered writes and
// periodic flushing, so that a log line costs the caller a copy and a channel
// send rather than a disk write. It only blocks when the queue is genuinely
// full, which means the disk has fallen a whole queue behind.
type AsyncFileWriter struct {
	file      *os.File
	writer    *bufio.Writer
	queue     chan []byte
	done      chan struct{}
	closeOnce sync.Once
	err       error

	// mu is held for reading by every Write and for writing by Close, which is
	// what makes closing the queue safe. Without it a Write that had already
	// passed the closed check could be left sending on a channel Close had
	// since shut, which panics — and the goroutines that outlive the listener
	// (a slow read dispatched off a session's read loop, a relay delivery, a
	// pion callback) are exactly the ones still logging while Close runs.
	mu     sync.RWMutex
	closed bool
}

// NewAsyncFileWriter creates an asynchronous file writer for the target path.
// Any missing parent directories are created automatically.
func NewAsyncFileWriter(path string) (*AsyncFileWriter, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}

	w := &AsyncFileWriter{
		file:   f,
		writer: bufio.NewWriterSize(f, defaultWriteBuffer),
		queue:  make(chan []byte, defaultQueueSize),
		done:   make(chan struct{}),
	}

	go w.worker()

	return w, nil
}

func (w *AsyncFileWriter) worker() {
	ticker := time.NewTicker(flushInterval)
	defer func() {
		ticker.Stop()
		if err := w.writer.Flush(); err != nil && w.err == nil {
			w.err = err
		}
		_ = w.file.Sync()
		close(w.done)
	}()

	for {
		select {
		case data, ok := <-w.queue:
			if !ok {
				// Drained
				return
			}
			_, _ = w.writer.Write(data)
			// If the queue is currently empty, flush right away so logs appear promptly.
			if len(w.queue) == 0 {
				_ = w.writer.Flush()
			}
		case <-ticker.C:
			_ = w.writer.Flush()
		}
	}
}

// Write enqueues a log slice to be written asynchronously to disk. Writing to
// a closed writer reports io.ErrClosedPipe rather than panicking.
func (w *AsyncFileWriter) Write(p []byte) (n int, err error) {
	// Copied before the lock: slog hands back the record buffer as soon as
	// this returns, so the queue cannot hold the caller's slice.
	cp := make([]byte, len(p))
	copy(cp, p)

	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	w.queue <- cp
	return len(p), nil
}

// Close drains all queued writes, flushes the buffer to disk, and closes the underlying file.
func (w *AsyncFileWriter) Close() error {
	var closeErr error
	w.closeOnce.Do(func() {
		// Taking the lock for writing waits out the Writes already in flight —
		// a full queue is drained by the worker, which takes no lock — and
		// stops the ones that have not started, so nothing is left sending on
		// the channel by the time it is closed.
		w.mu.Lock()
		w.closed = true
		close(w.queue)
		w.mu.Unlock()

		<-w.done
		if w.err != nil {
			closeErr = w.err
		}
		if err := w.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	})
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return closeErr
	}
	return nil
}
