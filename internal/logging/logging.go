// Package logging configures structured logging in the format that the
// Prometheus exporter ecosystem writes.
//
// These binaries run next to zfs_exporter, node_exporter and the others, and
// their output goes to the same place: a pod log, read by the same person or
// parsed by the same pipeline. One shared format needs one set of parsing
// rules, instead of a separate rule for each component:
//
//	time=2026-08-30T03:25:11.210Z level=INFO source=main.go:76 msg="..." key=value
//
// That format is the TextHandler of log/slog, which prometheus/common/promslog
// wraps. This package makes two changes to it. slog writes local time with a
// numeric offset, and it writes the source as an absolute path. The exporters
// write UTC to the millisecond, and a bare file name.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"
)

// timeFormat is RFC3339, truncated to milliseconds and pinned to UTC, which is
// what the exporters write. A reader sees the pod logs of several components
// together. A mixture of time offsets therefore makes those lines harder to put
// in order.
const timeFormat = "2006-01-02T15:04:05.000Z"

// New returns a logger that writes to w. Callers pass os.Stderr. Tests pass a
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
		// The absolute path belongs to the build machine, and a reader cannot
		// act on it. The file name and the line number locate the statement.
		if s, ok := a.Value.Any().(*slog.Source); ok {
			a.Value = slog.StringValue(fmt.Sprintf("%s:%d", filepath.Base(s.File), s.Line))
		}
	}
	return a
}
