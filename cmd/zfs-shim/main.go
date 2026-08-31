// Command zfs-shim runs zpool(8) and zfs(8) from the bundled OpenZFS userland
// that matches the kernel module on this host.
//
// zfs_exporter runs bare "zpool" and "zfs", which it resolves on PATH. This
// binary is installed on PATH under both names. At run time it reads the host
// module version from /sys/module/zfs/version. A container can read that file
// with no mount, because module state is not namespaced. The shim then
// executes the matching tree under /opt/zfs/<major>.<minor>.
//
// Each tree carries its own loader and libraries. The shim therefore starts a
// tree as
//
//	/opt/zfs/2.4/lib/ld-linux-x86-64.so.2 \
//	    --library-path /opt/zfs/2.4/lib /opt/zfs/2.4/sbin/zpool ...
//
// and it uses nothing installed at a standard path. That is what lets two
// userlands built against different glibc versions live in one image.
//
// The shim execs the command instead of forking. It replaces itself, so the
// exporter reads the exit status and the output of the real command
// directly.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/tschuering/zfs-exporter-helm/internal/logging"
)

const (
	treeRoot      = "/opt/zfs"
	versionSource = "/sys/module/zfs/version"
	// Replaces the detected version. It is for tests, and for a node where
	// /sys is not at the expected path.
	versionEnv = "ZFS_USERLAND_VERSION"
)

// Matches the leading major.minor of strings like "2.3.2-2", "2.4.4-1" or
// "2.3.99-1~exp1".
var majorMinor = regexp.MustCompile(`^(\d+)\.(\d+)`)

func main() {
	slog.SetDefault(logging.New(os.Stderr))

	loader, argv, err := execArgv(
		treeRoot, versionSource, filepath.Base(os.Args[0]), os.Args[1:],
	)
	if err != nil {
		fatal(err)
	}

	// The exec of a computed path is the purpose of this program. The path
	// points into a tree that the image carries under /opt/zfs, and not to
	// user input.
	//nolint:gosec // G204: the shim exists to exec the selected zpool or zfs
	if err := syscall.Exec(loader, argv, os.Environ()); err != nil {
		// argv[3] is the real command that the loader starts.
		fatal(fmt.Errorf("exec %s: %w", argv[3], err))
	}
}

// execArgv selects the tree under root that matches the host version from
// source. It returns the tree's loader, and the argument vector that makes
// the loader run the named command with the given args.
func execArgv(root, source, name string, args []string) (string, []string, error) {
	if name != "zpool" && name != "zfs" {
		return "", nil, fmt.Errorf("invoked as %q; expected zpool or zfs", name)
	}

	version, err := detectVersion(source)
	if err != nil {
		return "", nil, err
	}

	tree := filepath.Join(root, version)
	if _, err := os.Stat(tree); err != nil {
		// A permission or I/O error is not a missing userland. The message
		// that follows sends a reader after the wrong thing. Thus this branch
		// reports what actually happened.
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("stat %s: %w", tree, err)
		}

		return "", nil, fmt.Errorf(
			"no bundled OpenZFS userland for host version %s; this image carries %s",
			version, strings.Join(available(root), ", "),
		)
	}

	loader, err := findLoader(tree)
	if err != nil {
		return "", nil, err
	}

	target := filepath.Join(tree, "sbin", name)
	argv := append([]string{
		loader, "--library-path", filepath.Join(tree, "lib"), target,
	}, args...)

	return loader, argv, nil
}

// detectVersion returns the major.minor of the host's ZFS kernel module.
func detectVersion(source string) (string, error) {
	if v := os.Getenv(versionEnv); v != "" {
		version, ok := parseVersion(v)
		if !ok {
			return "", fmt.Errorf("%s=%q is not a version", versionEnv, v)
		}

		return version, nil
	}

	//nolint:gosec // G304: source is the constant versionSource, or a path from a test
	raw, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf(
			"%s does not exist: the ZFS kernel module is not loaded on this "+
				"node, so there is nothing to report on", source,
		)
	}

	if err != nil {
		return "", fmt.Errorf("reading %s: %w", source, err)
	}

	v := strings.TrimSpace(string(raw))

	version, ok := parseVersion(v)
	if !ok {
		return "", fmt.Errorf("%s contains %q, which is not a version",
			source, v)
	}

	return version, nil
}

// parseVersion returns the leading major.minor of v, and reports whether v
// carries one. The two callers of this function differ only in the message
// that they give for a value with no version in it.
func parseVersion(v string) (string, bool) {
	m := majorMinor.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}

	return m[1] + "." + m[2], true
}

// findLoader returns the tree's own dynamic loader. The name of that loader
// is architecture-specific: ld-linux-x86-64.so.2, ld-linux-aarch64.so.1, and
// so on.
func findLoader(tree string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(tree, "lib", "ld-*.so*"))
	if err != nil {
		return "", err
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no dynamic loader in %s/lib", tree)
	}

	slices.Sort(matches)

	return matches[0], nil
}

// available lists the userland versions that this image carries.
func available(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []string{"none"}
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}

	if len(out) == 0 {
		return []string{"none"}
	}

	slices.Sort(out)

	return out
}

func fatal(err error) {
	slog.Error("Cannot dispatch to a bundled OpenZFS userland", "err", err)
	os.Exit(1)
}
