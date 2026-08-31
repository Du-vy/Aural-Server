// Package logging builds the structured logger the server writes through.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options specifies configuration for the server's structured logging system.
type Options struct {
	Level      string    // "debug", "info", "warn", "error" (default: "info")
	Format     string    // "pretty", "text", "json" (default: "pretty")
	File       string    // file path (e.g. "logs/aural.log") or empty
	FileLevel  string    // file log level (defaults to Level if empty)
	FileFormat string    // "json", "text", "pretty" (default: "json")
	NoColor    bool      // disable ANSI color escape codes
	Out        io.Writer // custom console destination (defaults to os.Stderr)
}

// Setup builds and returns the configured slog.Logger along with an io.Closer
// that must be closed on server shutdown to flush any asynchronous file writes.
func Setup(opts Options) (*slog.Logger, io.Closer, error) {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	consoleLevel := parseLevel(opts.Level)
	consoleOpts := &slog.HandlerOptions{Level: consoleLevel}

	var consoleHandler slog.Handler
	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "json":
		consoleHandler = slog.NewJSONHandler(out, consoleOpts)
	case "text":
		consoleHandler = slog.NewTextHandler(out, consoleOpts)
	default: // "pretty" or anything else
		consoleHandler = NewConsoleHandler(out, consoleOpts, opts.NoColor)
	}

	var closers []io.Closer
	var handlers = []slog.Handler{consoleHandler}

	if opts.File != "" {
		fileWriter, err := NewAsyncFileWriter(opts.File)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, fileWriter)

		fileLevelStr := opts.FileLevel
		if fileLevelStr == "" {
			fileLevelStr = opts.Level
		}
		fileOpts := &slog.HandlerOptions{Level: parseLevel(fileLevelStr)}

		var fileHandler slog.Handler
		switch strings.ToLower(strings.TrimSpace(opts.FileFormat)) {
		case "text":
			fileHandler = slog.NewTextHandler(fileWriter, fileOpts)
		case "pretty":
			fileHandler = NewConsoleHandler(fileWriter, fileOpts, true) // Force no ANSI in file
		default: // "json" by default for files
			fileHandler = slog.NewJSONHandler(fileWriter, fileOpts)
		}

		handlers = append(handlers, fileHandler)
	}

	logger := slog.New(NewMultiHandler(handlers...))
	closer := &compositeCloser{closers: closers}
	return logger, closer, nil
}

// New returns a logger for the configured level and format. Unknown values fall
// back to info and pretty rather than failing. Provided for backwards compatibility.
func New(level, format string) *slog.Logger {
	log, _, _ := Setup(Options{Level: level, Format: format})
	return log
}

// ParseLevel parses a level string into slog.Level.
func ParseLevel(level string) slog.Level {
	return parseLevel(level)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type compositeCloser struct {
	closers []io.Closer
}

func (c *compositeCloser) Close() error {
	var errs []error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
