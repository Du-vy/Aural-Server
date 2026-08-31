package logging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultQueueSize   = 4096
	defaultWriteBuffer = 64 * 1024
	flushInterval      = 250 * time.Millisecond
)

// AsyncFileWriter is a high-performance, non-blocking asynchronous file sink
// with buffered writes and periodic flushing to minimize disk I/O latency.
type AsyncFileWriter struct {
	file      *os.File
	writer    *bufio.Writer
	queue     chan []byte
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	err       error
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

// Write enqueues a log slice to be written asynchronously to disk.
func (w *AsyncFileWriter) Write(p []byte) (n int, err error) {
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	cp := make([]byte, len(p))
	copy(cp, p)

	// Enqueue write
	w.queue <- cp
	return len(p), nil
}

// Close drains all queued writes, flushes the buffer to disk, and closes the underlying file.
func (w *AsyncFileWriter) Close() error {
	var closeErr error
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		close(w.queue)
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
