package logging

import (
	"context"
	"errors"
	"log/slog"
)

// MultiHandler multiplexes slog records across multiple handlers.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler creates a handler that sends records to all provided handlers.
func NewMultiHandler(handlers ...slog.Handler) slog.Handler {
	var valid []slog.Handler
	for _, h := range handlers {
		if h != nil {
			valid = append(valid, h)
		}
	}
	if len(valid) == 0 {
		return slog.DiscardHandler
	}
	if len(valid) == 1 {
		return valid[0]
	}
	return &MultiHandler{handlers: valid}
}

// Enabled returns true if ANY underlying handler is enabled for this level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle forwards the record to each handler that is enabled for record.Level.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a MultiHandler with WithAttrs called on each child handler.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: handlers}
}

// WithGroup returns a MultiHandler with WithGroup called on each child handler.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}
