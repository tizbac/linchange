// Package logging configures the process-wide structured logger.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/linchange/linchange/config"
)

// New builds a logger from the logging section of the configuration.
//
// Records go to stderr, to a file when one is configured, or to both. Writing
// to both by default means a container runtime collecting stderr keeps working
// even if the log file becomes unwritable.
func New(cfg config.Log, service string) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	format := strings.ToLower(cfg.Format)
	if format != "json" && format != "text" {
		return nil, fmt.Errorf("logging: format %q must be json or text", cfg.Format)
	}

	writers := []io.Writer{os.Stderr}
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("logging: opening %s: %w", cfg.File, err)
		}
		writers = append(writers, f)
	}

	opts := &slog.HandlerOptions{
		Level: level,
		// Record the source file and line for warnings and above, where the
		// origin of the message is what an operator actually needs.
		AddSource: level <= slog.LevelWarn,
	}

	handlers := make([]slog.Handler, 0, len(writers))
	for _, w := range writers {
		handlers = append(handlers, newHandler(format, w, opts))
	}

	var handler slog.Handler = &multiHandler{sinks: handlers}
	if len(handlers) == 1 {
		handler = handlers[0]
	}

	return slog.New(handler.WithAttrs([]slog.Attr{slog.String("service", service)})), nil
}

func newHandler(format string, w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	if format == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: level %q must be one of debug, info, warn, error", s)
	}
}

// multiHandler fans one record out to several sinks. slog has no built-in
// equivalent.
type multiHandler struct {
	sinks []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.sinks {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.sinks {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Clone per sink: a handler may retain the record, and sharing one
		// record across sinks would let the first handler's formatting
		// interfere with the second.
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	sinks := make([]slog.Handler, 0, len(m.sinks))
	for _, h := range m.sinks {
		sinks = append(sinks, h.WithAttrs(attrs))
	}
	return &multiHandler{sinks: sinks}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	sinks := make([]slog.Handler, 0, len(m.sinks))
	for _, h := range m.sinks {
		sinks = append(sinks, h.WithGroup(name))
	}
	return &multiHandler{sinks: sinks}
}
