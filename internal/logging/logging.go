// Package logging installs the process logger for serve mode.
//
// The codebase logs through the standard library's log package. Rewriting every
// call site into structured fields is a much larger change than the value it
// would add; instead this package installs a slog handler and bridges the
// standard logger into it. JSON mode therefore turns the existing lines into
// {"time":…,"level":"INFO","msg":"…"} without losing any of them, while new
// code (the request access log) uses slog directly.
package logging

import (
	"fmt"
	"io"
	"log"
	"log/slog"
)

// Install configures the default slog logger and the standard log bridge.
// format is "text" (or empty) or "json".
func Install(format string, w io.Writer) error {
	var handler slog.Handler
	switch format {
	case "", "text":
		handler = slog.NewTextHandler(w, nil)
	case "json":
		handler = slog.NewJSONHandler(w, nil)
	default:
		return fmt.Errorf("unknown log format %q (want text|json)", format)
	}
	slog.SetDefault(slog.New(handler))
	// Every existing log.Printf lands in the same handler as an INFO line. The
	// old prefix/flags are cleared so JSON gets one timestamp from slog.
	log.SetOutput(slog.NewLogLogger(handler, slog.LevelInfo).Writer())
	log.SetFlags(0)
	log.SetPrefix("")
	return nil
}
