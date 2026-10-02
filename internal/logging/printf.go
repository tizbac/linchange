package logging

import (
	"fmt"
	"log/slog"
	"strings"
)

// Printf adapts a slog.Logger to the Printf-style sink that libraries such as
// go-smtp expect, so their diagnostics land in the same structured log as
// everything else.
//
// go-smtp calls this for accept errors, per-connection errors and panics, and
// it holds the value unconditionally: leaving it nil panics the server on the
// first client that disconnects.
type Printf struct {
	logger *slog.Logger
}

// NewPrintf wraps log for use as a library diagnostic sink. A nil logger falls
// back to the default so that the sink is never nil.
func NewPrintf(log *slog.Logger) *Printf {
	if log == nil {
		log = slog.Default()
	}
	return &Printf{logger: log}
}

// Printf formats and logs one library diagnostic.
func (p *Printf) Printf(format string, args ...any) {
	p.write(fmt.Sprintf(format, args...))
}

// Println logs one library diagnostic written without a format string.
func (p *Printf) Println(args ...any) {
	p.write(strings.TrimSuffix(fmt.Sprintln(args...), "\n"))
}

func (p *Printf) write(msg string) {
	msg = strings.TrimSpace(msg)
	if strings.Contains(msg, "panic") {
		p.logger.Error(msg)
		return
	}
	p.logger.Warn(msg)
}
