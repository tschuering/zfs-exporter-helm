package logging

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// This project does not choose the format freely. Every other exporter in the
// namespace writes this format, and a pipeline that parses those lines should
// not need a second rule for these binaries. This test therefore pins the
// format, not only the fact that the code logs.
//
//	time=2026-08-30T03:25:11.210Z level=INFO source=zfs_exporter.go:76 msg="Enabling collectors" collectors="..."
var wantShape = regexp.MustCompile(
	`^time=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z ` +
		`level=INFO ` +
		`source=logging_test\.go:\d+ ` +
		`msg="Enabling collectors" ` +
		`collectors="dataset-filesystem, dataset-volume, pool"\n$`)

func TestFormatMatchesTheExporters(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Info("Enabling collectors",
		"collectors", "dataset-filesystem, dataset-volume, pool")

	got := buf.String()
	if !wantShape.MatchString(got) {
		t.Errorf("log line does not match the exporter format.\n got: %s\nwant: %s", got, wantShape)
	}
}

// The time is UTC to the millisecond, with a literal Z. The default in slog is
// local time with a numeric offset, which this package corrects.
func TestTimeIsUTCMilliseconds(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Info("x")

	field := strings.SplitN(buf.String(), " ", 2)[0]
	if !strings.HasSuffix(field, "Z") {
		t.Errorf("time is not UTC: %s", field)
	}
	if got := len(strings.SplitN(field, ".", 2)[1]); got != 4 { // "210Z"
		t.Errorf("time has %d digits after the dot, want milliseconds: %s", got-1, field)
	}
}

// slog reports the absolute path of the source file. That path is the layout
// of the build machine, and a reader cannot use it.
func TestSourceIsBareFileName(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Error("boom")

	for _, field := range strings.Fields(buf.String()) {
		if strings.HasPrefix(field, "source=") {
			if strings.Contains(field, "/") {
				t.Errorf("source should be a bare file name, got %s", field)
			}
			return
		}
	}
	t.Fatal("no source= field; AddSource should be on")
}

func TestLevelsAreNamed(t *testing.T) {
	for _, tc := range []struct {
		log  func(*bytes.Buffer)
		want string
	}{
		{func(b *bytes.Buffer) { New(b).Info("x") }, "level=INFO"},
		{func(b *bytes.Buffer) { New(b).Warn("x") }, "level=WARN"},
		{func(b *bytes.Buffer) { New(b).Error("x") }, "level=ERROR"},
	} {
		var buf bytes.Buffer
		tc.log(&buf)
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("want %s in %q", tc.want, buf.String())
		}
	}
}
