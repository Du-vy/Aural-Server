package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// ANSI color codes.
const (
	ansiReset        = "\x1b[0m"
	ansiBold         = "\x1b[1m"
	ansiDim          = "\x1b[2m"
	ansiGray         = "\x1b[90m"
	ansiRed          = "\x1b[31m"
	ansiBoldRed      = "\x1b[1;31m"
	ansiGreen        = "\x1b[32m"
	ansiYellow       = "\x1b[33m"
	ansiBlue         = "\x1b[34m"
	ansiMagenta      = "\x1b[35m"
	ansiCyan         = "\x1b[36m"
	ansiBrightBlue   = "\x1b[94m"
	ansiBrightCyan   = "\x1b[96m"
)

// ConsoleHandler is a slog.Handler designed for human-readable terminal output
// with distinctive colors, aligned columns, and clear attribute formatting.
type ConsoleHandler struct {
	opts    *slog.HandlerOptions
	out     io.Writer
	mu      *sync.Mutex
	noColor bool
	groups  []string
	attrs   []slog.Attr
}

// NewConsoleHandler returns a new ConsoleHandler writing to out with the given options.
func NewConsoleHandler(out io.Writer, opts *slog.HandlerOptions, forceNoColor bool) *ConsoleHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	if out == nil {
		out = os.Stderr
	}

	noColor := forceNoColor
	if !noColor {
		if os.Getenv("NO_COLOR") != "" {
			noColor = true
		} else if f, ok := out.(*os.File); ok {
			enableVirtualTerminal(f)
			if !isatty.IsTerminal(f.Fd()) && !isatty.IsCygwinTerminal(f.Fd()) {
				noColor = true
			}
		}
	}

	return &ConsoleHandler{
		opts:    opts,
		out:     out,
		mu:      &sync.Mutex{},
		noColor: noColor,
	}
}

// Enabled reports whether the handler handles records at the given level.
func (h *ConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

// Handle formats and writes a record to the output writer.
func (h *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	var buf bytes.Buffer

	// 1. Timestamp: HH:MM:SS (in dim gray)
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	timeStr := ts.Format("15:04:05")
	if h.noColor {
		buf.WriteString(timeStr)
		buf.WriteByte(' ')
	} else {
		buf.WriteString(ansiGray)
		buf.WriteString(timeStr)
		buf.WriteString(ansiReset)
		buf.WriteByte(' ')
	}

	// 2. Level Badge: [DEBUG], [INFO ], [WARN ], [ERROR]
	levelStr := formatLevel(r.Level, h.noColor)
	buf.WriteString(levelStr)
	buf.WriteByte(' ')

	// 3. Message
	if h.noColor {
		buf.WriteString(r.Message)
	} else {
		buf.WriteString(ansiBold)
		buf.WriteString(r.Message)
		buf.WriteString(ansiReset)
	}

	// 4. Attributes (pre-set handler attributes + record attributes)
	groupPrefix := ""
	if len(h.groups) > 0 {
		groupPrefix = strings.Join(h.groups, ".") + "."
	}

	// Write pre-set attrs
	for _, attr := range h.attrs {
		h.writeAttr(&buf, groupPrefix, attr)
	}

	// Write record attrs
	r.Attrs(func(attr slog.Attr) bool {
		h.writeAttr(&buf, groupPrefix, attr)
		return true
	})

	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(buf.Bytes())
	return err
}

func (h *ConsoleHandler) writeAttr(buf *bytes.Buffer, groupPrefix string, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		newPrefix := groupPrefix + attr.Key + "."
		for _, a := range attr.Value.Group() {
			h.writeAttr(buf, newPrefix, a)
		}
		return
	}

	buf.WriteByte(' ')
	key := groupPrefix + attr.Key

	if h.noColor {
		buf.WriteString(key)
		buf.WriteByte('=')
		buf.WriteString(formatValue(attr.Key, attr.Value, true))
	} else {
		buf.WriteString(ansiCyan)
		buf.WriteString(key)
		buf.WriteString(ansiReset)
		buf.WriteString(ansiGray)
		buf.WriteByte('=')
		buf.WriteString(ansiReset)
		buf.WriteString(formatValue(attr.Key, attr.Value, false))
	}
}

func formatLevel(level slog.Level, noColor bool) string {
	var tag, color string
	switch {
	case level < slog.LevelInfo:
		tag = "DEBUG"
		color = ansiMagenta
	case level < slog.LevelWarn:
		tag = "INFO "
		color = ansiGreen
	case level < slog.LevelError:
		tag = "WARN "
		color = ansiYellow
	default:
		tag = "ERROR"
		color = ansiBoldRed
	}

	if noColor {
		return tag
	}
	return color + tag + ansiReset
}

func formatValue(key string, val slog.Value, noColor bool) string {
	kind := val.Kind()
	switch kind {
	case slog.KindString:
		s := val.String()
		// If key contains err/error, color in red
		if !noColor && (strings.Contains(key, "err") || strings.Contains(key, "error")) {
			return ansiRed + quoteIfNeeded(s) + ansiReset
		}
		return quoteIfNeeded(s)
	case slog.KindInt64:
		s := strconv.FormatInt(val.Int64(), 10)
		if noColor {
			return s
		}
		return ansiYellow + s + ansiReset
	case slog.KindUint64:
		s := strconv.FormatUint(val.Uint64(), 10)
		if noColor {
			return s
		}
		return ansiYellow + s + ansiReset
	case slog.KindFloat64:
		s := strconv.FormatFloat(val.Float64(), 'g', -1, 64)
		if noColor {
			return s
		}
		return ansiYellow + s + ansiReset
	case slog.KindBool:
		s := strconv.FormatBool(val.Bool())
		if noColor {
			return s
		}
		return ansiMagenta + s + ansiReset
	case slog.KindDuration:
		s := val.Duration().String()
		if noColor {
			return s
		}
		return ansiYellow + s + ansiReset
	case slog.KindTime:
		s := val.Time().Format(time.RFC3339)
		if noColor {
			return s
		}
		return ansiBrightCyan + s + ansiReset
	case slog.KindAny:
		anyVal := val.Any()
		if err, ok := anyVal.(error); ok {
			s := err.Error()
			if noColor {
				return quoteIfNeeded(s)
			}
			return ansiRed + quoteIfNeeded(s) + ansiReset
		}
		return quoteIfNeeded(fmt.Sprint(anyVal))
	default:
		return quoteIfNeeded(val.String())
	}
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\r\"=") {
		return strconv.Quote(s)
	}
	return s
}

// WithAttrs returns a new ConsoleHandler with attrs added.
func (h *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	newAttrs = append(newAttrs, h.attrs...)
	newAttrs = append(newAttrs, attrs...)
	return &ConsoleHandler{
		opts:    h.opts,
		out:     h.out,
		mu:      h.mu,
		noColor: h.noColor,
		groups:  h.groups,
		attrs:   newAttrs,
	}
}

// WithGroup returns a new ConsoleHandler with a group name added.
func (h *ConsoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	newGroups := make([]string, 0, len(h.groups)+1)
	newGroups = append(newGroups, h.groups...)
	newGroups = append(newGroups, name)
	return &ConsoleHandler{
		opts:    h.opts,
		out:     h.out,
		mu:      h.mu,
		noColor: h.noColor,
		groups:  newGroups,
		attrs:   h.attrs,
	}
}
