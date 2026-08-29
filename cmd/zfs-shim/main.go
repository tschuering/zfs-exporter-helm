// Command zfs-shim dispatches zpool(8) and zfs(8) to whichever bundled
// OpenZFS userland matches the kernel module running on this host.
//
// zfs_exporter shells out to bare "zpool" and "zfs", resolved on PATH. This
// binary is installed on PATH under both names and, when run, reads the host
// module version out of /sys/module/zfs/version -- visible inside a container
// without any mount, because module state is not namespaced -- and executes
// the corresponding tree under /opt/zfs/<major>.<minor>.
//
// Each tree carries its own loader and libraries, so it is started as
//
//	/opt/zfs/2.4/lib/ld-linux-x86-64.so.2 \
//	    --library-path /opt/zfs/2.4/lib /opt/zfs/2.4/sbin/zpool ...
//
// rather than relying on anything installed at a standard path. That is what
// lets two userlands built against different glibc versions coexist.
//
// Exec, not fork: the shim replaces itself, so the exporter sees the real
// command's exit status and output with nothing in between.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const (
	treeRoot      = "/opt/zfs"
	versionSource = "/sys/module/zfs/version"
	// Overrides the detected version. For testing, and for the case where
	// /sys is not where we expect it.
	versionEnv = "ZFS_USERLAND_VERSION"
)

// Matches the leading major.minor of strings like "2.3.2-2", "2.4.4-1" or
// "2.3.99-1~exp1".
var majorMinor = regexp.MustCompile(`^(\d+)\.(\d+)`)

func main() {
	name := filepath.Base(os.Args[0])
	if name != "zpool" && name != "zfs" {
		fatal(fmt.Errorf("invoked as %q; expected zpool or zfs", name))
	}

	version, err := detectVersion()
	if err != nil {
		fatal(err)
	}

	tree := filepath.Join(treeRoot, version)
	if _, err := os.Stat(tree); err != nil {
		fatal(fmt.Errorf(
			"no bundled OpenZFS userland for host version %s; this image carries %s",
			version, strings.Join(available(), ", ")))
	}

	loader, err := findLoader(tree)
	if err != nil {
		fatal(err)
	}

	target := filepath.Join(tree, "sbin", name)
	argv := append([]string{
		loader, "--library-path", filepath.Join(tree, "lib"), target,
	}, os.Args[1:]...)

	if err := syscall.Exec(loader, argv, os.Environ()); err != nil {
		fatal(fmt.Errorf("exec %s: %w", target, err))
	}
}

// detectVersion returns the major.minor of the host's ZFS kernel module.
func detectVersion() (string, error) {
	if v := os.Getenv(versionEnv); v != "" {
		m := majorMinor.FindStringSubmatch(v)
		if m == nil {
			return "", fmt.Errorf("%s=%q is not a version", versionEnv, v)
		}
		return m[1] + "." + m[2], nil
	}

	raw, err := os.ReadFile(versionSource)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf(
			"%s does not exist: the ZFS kernel module is not loaded on this "+
				"node, so there is nothing to report on", versionSource)
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", versionSource, err)
	}

	v := strings.TrimSpace(string(raw))
	m := majorMinor.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("%s contains %q, which is not a version",
			versionSource, v)
	}
	return m[1] + "." + m[2], nil
}

// findLoader returns the tree's own dynamic loader, whose name is
// architecture-specific (ld-linux-x86-64.so.2, ld-linux-aarch64.so.1, ...).
func findLoader(tree string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(tree, "lib", "ld-*.so*"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no dynamic loader in %s/lib", tree)
	}
	sort.Strings(matches)
	return matches[0], nil
}

// available lists the userland versions bundled into this image.
func available() []string {
	entries, err := os.ReadDir(treeRoot)
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
	sort.Strings(out)
	return out
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "zfs-shim: %v\n", err)
	os.Exit(1)
}
