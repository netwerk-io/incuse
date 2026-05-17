package scaleset

import (
	"io"
	"log/slog"
)

// discardLogger returns a slog.Logger that drops every record on the
// floor. Convenience for tests that don't care about log output but
// still need a non-nil logger.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
