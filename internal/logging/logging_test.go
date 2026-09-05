package logging

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConsoleHandlerFormatting(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true)
	logger := slog.New(h)

	logger.Info("server listening", slog.String("addr", "0.0.0.0:9871"), slog.Int("port", 9871))
	output := buf.String()

	if !strings.Contains(output, "INFO ") {
		t.Errorf("expected level tag INFO in output: %s", output)
	}
	if !strings.Contains(output, "server listening") {
		t.Errorf("expected message in output: %s", output)
	}
	if !strings.Contains(output, "addr=0.0.0.0:9871") {
		t.Errorf("expected addr attribute: %s", output)
	}
	if !strings.Contains(output, "port=9871") {
		t.Errorf("expected port attribute: %s", output)
	}
}

func TestConsoleHandlerAttributesAndGroups(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true)
	logger := slog.New(h).With("service", "gateway").WithGroup("http")

	logger.Error("request failed", slog.String("path", "/ws"), slog.Any("err", errors.New("timeout")))
	output := buf.String()

	if !strings.Contains(output, "ERROR") {
		t.Errorf("expected ERROR level: %s", output)
	}
	if !strings.Contains(output, "service=gateway") {
		t.Errorf("expected service attribute: %s", output)
	}
	if !strings.Contains(output, "http.path=/ws") {
		t.Errorf("expected grouped path attribute: %s", output)
	}
	if !strings.Contains(output, "http.err=timeout") {
		t.Errorf("expected grouped err attribute: %s", output)
	}
}

func TestAsyncFileWriter(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "logs", "test.log")

	writer, err := NewAsyncFileWriter(logPath)
	if err != nil {
		t.Fatalf("NewAsyncFileWriter: %v", err)
	}

	lines := []string{
		"line 1: hello world\n",
		"line 2: server started\n",
		"line 3: shutting down\n",
	}

	var wg sync.WaitGroup
	for _, line := range lines {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			_, _ = writer.Write([]byte(l))
		}(line)
	}
	wg.Wait()

	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	content := string(data)
	for _, line := range lines {
		if !strings.Contains(content, strings.TrimSpace(line)) {
			t.Errorf("missing %q in log file content: %s", line, content)
		}
	}
}

// TestAsyncFileWriterWritesDuringClose covers the shutdown ordering the server
// actually has: the goroutines that outlive the listener — a slow read
// dispatched off a session's read loop, a relay delivery, a pion callback —
// are still logging while main closes the sink. A Write that passed the closed
// check and then sent on a channel Close had shut would take the process down
// with it, which is the one failure a logger must not have.
func TestAsyncFileWriterWritesDuringClose(t *testing.T) {
	writer, err := NewAsyncFileWriter(filepath.Join(t.TempDir(), "test.log"))
	if err != nil {
		t.Fatalf("NewAsyncFileWriter: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				// Both outcomes are correct: accepted before the close, or
				// refused after it. Neither may panic.
				if _, err := writer.Write([]byte("still going\n")); err != nil &&
					!errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}

	close(start)
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	if _, err := writer.Write([]byte("after close\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Write after Close = %v, want io.ErrClosedPipe", err)
	}
	if err := writer.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSetupMultiHandler(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "aural.log")

	var consoleBuf bytes.Buffer
	logger, closer, err := Setup(Options{
		Level:      "info",
		Format:     "pretty",
		File:       logFile,
		FileLevel:  "debug",
		FileFormat: "json",
		NoColor:    true,
		Out:        &consoleBuf,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer closer.Close()

	logger.Debug("debug message")
	logger.Info("info message", slog.String("key", "val"))

	// Flush file writer
	if err := closer.Close(); err != nil {
		t.Fatalf("closer.Close: %v", err)
	}

	// Console should only have info (min level info)
	consoleOut := consoleBuf.String()
	if strings.Contains(consoleOut, "debug message") {
		t.Errorf("console should not have debug message: %s", consoleOut)
	}
	if !strings.Contains(consoleOut, "info message") {
		t.Errorf("console should have info message: %s", consoleOut)
	}

	// File should have BOTH debug and info because FileLevel is "debug"
	fileData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	fileContent := string(fileData)
	if !strings.Contains(fileContent, "debug message") {
		t.Errorf("file should have debug message: %s", fileContent)
	}
	if !strings.Contains(fileContent, "info message") {
		t.Errorf("file should have info message: %s", fileContent)
	}
}
