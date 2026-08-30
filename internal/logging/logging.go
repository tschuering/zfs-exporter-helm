// Package logging configures structured logging that matches what the
// Prometheus exporter ecosystem emits.
//
// These binaries sit next to zfs_exporter, node_exporter and the rest, and
// their output lands in the same place -- a pod log, read by the same eye or
// parsed by the same pipeline. Matching the shape means one set of parsing
// rules rather than a special case per component:
//
//	time=2026-08-30T03:25:11.210Z level=INFO source=main.go:76 msg="..." key=value
//
// That is log/slog's TextHandler, which is what prometheus/common/promslog
// wraps, with two adjustments. slog writes local time with a numeric offset
// and the source as an absolute path; the exporters write UTC to the
// millisecond and a bare file name.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"
)

// timeFormat is RFC3339 truncated to milliseconds and pinned to UTC, which is
// what the exporters emit. Pod logs from several components are read
// interleaved, so a mixture of offsets makes ordering them by eye harder than
// it needs to be.
const timeFormat = "2006-01-02T15:04:05.000Z"

// New returns a logger writing to w. Callers pass os.Stderr; tests pass a
// buffer.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		AddSource:   true,
		ReplaceAttr: replace,
	}))
}

func replace(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.UTC().Format(timeFormat))
		}
	case slog.SourceKey:
		// The absolute path is the build machine's, not anything a reader can
		// act on. The file name and line are what locate the statement.
		if s, ok := a.Value.Any().(*slog.Source); ok {
			a.Value = slog.StringValue(fmt.Sprintf("%s:%d", filepath.Base(s.File), s.Line))
		}
	}
	return a
}
