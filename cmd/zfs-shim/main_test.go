package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// One name for each version branch that the tests use.
const (
	v23 = "2.3"
	v24 = "2.4"
)

// The shim selects the bundled OpenZFS userland that runs against the host's
// kernel module. A wrong selection does not fail loudly. It runs a 2.3 zpool
// against a 2.4 module, or a 2.4 zpool against a 2.3 module. The failure then
// appears as an ioctl error, which reads like a ZFS problem and not like a
// parsing problem. These tests therefore pin down the parsing.
func TestDetectVersionFromFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "debian package version", contents: "2.3.2-2\n", want: v23},
		{name: "forky", contents: "2.4.4-1\n", want: v24},
		{name: "no packaging suffix", contents: "2.3.9\n", want: v23},
		{name: "experimental suffix", contents: "2.3.99-1~exp1\n", want: v23},
		{name: "leading and trailing space", contents: "  2.4.0-rc1  \n", want: v24},
		{name: "double digit minor", contents: "2.10.1-1\n", want: "2.10"},
		{name: "empty", contents: "\n", wantErr: true},
		{name: "not a version", contents: "unknown\n", wantErr: true},
		{name: "major only", contents: "2\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := filepath.Join(t.TempDir(), "version")
			if err := os.WriteFile(source, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := detectVersion(source)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("detectVersion(%q) = %q, want an error", tc.contents, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("detectVersion(%q): %v", tc.contents, err)
			}

			if got != tc.want {
				t.Errorf("detectVersion(%q) = %q, want %q", tc.contents, got, tc.want)
			}
		})
	}
}

// treeFixture builds a root with one tree for the given version, and a
// version file that selects that tree. The tree carries a loader, which is
// all that execArgv examines.
func treeFixture(t *testing.T, version string) (string, string) {
	t.Helper()

	dir := t.TempDir()

	root := filepath.Join(dir, "opt")

	lib := filepath.Join(root, version, "lib")
	if err := os.MkdirAll(lib, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(lib, "ld-linux-x86-64.so.2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "version")
	if err := os.WriteFile(source, []byte(version+".2-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return root, source
}

// The argv decides what actually runs. The loader must come first,
// --library-path must point into the tree, and the caller's arguments must
// follow the target unchanged.
func TestExecArgv(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"zpool", "zfs"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, source := treeFixture(t, v23)

			loader, argv, err := execArgv(root, source, name, []string{"status", "-x"})
			if err != nil {
				t.Fatal(err)
			}

			if filepath.Base(loader) != "ld-linux-x86-64.so.2" {
				t.Errorf("loader = %q, want the loader of the fixture", loader)
			}

			tree := filepath.Join(root, v23)
			want := []string{
				loader, "--library-path", filepath.Join(tree, "lib"),
				filepath.Join(tree, "sbin", name), "status", "-x",
			}

			if !slices.Equal(argv, want) {
				t.Errorf("argv = %q, want %q", argv, want)
			}
		})
	}
}

// The shim replaces zpool and zfs on PATH, and nothing else. A link under
// another name is an installation mistake, and the message must say so.
func TestExecArgvRejectsOtherNames(t *testing.T) {
	t.Parallel()

	root, source := treeFixture(t, v23)

	_, _, err := execArgv(root, source, "cp", nil)
	if err == nil || !strings.Contains(err.Error(), "expected zpool or zfs") {
		t.Errorf("execArgv as \"cp\" = %v, want the name error", err)
	}
}

// For a host version outside the bundled set, the error must name the trees
// that are present. That list tells the reader which image to use instead.
func TestExecArgvUnbundledVersion(t *testing.T) {
	t.Parallel()

	root, _ := treeFixture(t, v23)

	source := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(source, []byte("9.9.0-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := execArgv(root, source, "zpool", nil)
	if err == nil || !strings.Contains(err.Error(), "no bundled OpenZFS userland") ||
		!strings.Contains(err.Error(), v23) {
		t.Errorf("execArgv = %v, want the unbundled error with the carried trees", err)
	}
}

// The errors of detectVersion and findLoader must pass through execArgv
// unchanged, so that the reader sees the original problem.
func TestExecArgvPassesHelperErrors(t *testing.T) {
	t.Parallel()

	root, _ := treeFixture(t, v23)

	if _, _, err := execArgv(root, filepath.Join(t.TempDir(), "absent"), "zpool", nil); err == nil {
		t.Error("execArgv with a missing version source must fail")
	}

	// A tree that carries no loader.
	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, v23, "lib"), 0o750); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(source, []byte("2.3.0-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := execArgv(bare, source, "zpool", nil)
	if err == nil || !strings.Contains(err.Error(), "no dynamic loader") {
		t.Errorf("execArgv on a tree without a loader = %v, want the loader error", err)
	}
}

// A permission problem on the tree is not a missing userland. The wrong
// message sends a reader after the wrong image, so this branch has its own
// test.
func TestExecArgvStatError(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	root, source := treeFixture(t, v23)

	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	// Without the restore, the cleanup of t.TempDir cannot remove the tree.
	// The removal of a directory needs all three owner bits on it.
	//nolint:gosec // G302: 0o700 is the minimum that lets the cleanup work
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	_, _, err := execArgv(root, source, "zpool", nil)
	if err == nil || !strings.Contains(err.Error(), "stat") {
		t.Errorf("execArgv = %v, want a stat error and not the unbundled error", err)
	}
}

// A node without the module is the normal case on a mixed cluster, and on
// every CI runner. The shim must report that in terms a reader can act on. It
// must not fail at a deeper level instead.
func TestDetectVersionMissingFile(t *testing.T) {
	t.Parallel()

	_, err := detectVersion(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("want an error when the version file does not exist")
	}

	if !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("error must say the module is not loaded, got: %v", err)
	}
}

// A source that exists but cannot be read is not a missing module. The error
// must name the read problem.
func TestDetectVersionUnreadableSource(t *testing.T) {
	t.Parallel()

	_, err := detectVersion(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Errorf("detectVersion on a directory = %v, want a read error", err)
	}
}

func TestDetectVersionEnvOverrides(t *testing.T) {
	// The override is for nodes where /sys is not at the path the shim
	// expects. It also lets a test run a tree other than the host's.
	source := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(source, []byte("2.3.2-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(versionEnv, "2.4.4-1")

	got, err := detectVersion(source)
	if err != nil {
		t.Fatal(err)
	}

	if got != v24 {
		t.Errorf("env override = %q, want 2.4 (the file said 2.3)", got)
	}

	t.Setenv(versionEnv, "nonsense")

	if _, err := detectVersion(source); err == nil {
		t.Error("a malformed override must fail, and must not use the file")
	}
}

// The error for an unbundled branch names the branches that the image does
// carry. The list must therefore show the trees that are present.
func TestAvailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, d := range []string{v24, v23} {
		if err := os.Mkdir(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file is not a tree.
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(available(root), ", ")
	if got != "2.3, 2.4" {
		t.Errorf("available() = %q, want \"2.3, 2.4\" (sorted, directories only)", got)
	}

	if got := strings.Join(available(filepath.Join(root, "absent")), ", "); got != "none" {
		t.Errorf("available() on a missing root = %q, want \"none\"", got)
	}

	// A root with no directories in it is as empty as a missing root.
	if got := strings.Join(available(t.TempDir()), ", "); got != "none" {
		t.Errorf("available() on an empty root = %q, want \"none\"", got)
	}
}

// Each tree carries its own loader, and the name of that loader is
// architecture-specific. The glob is what lets the shim run on amd64 and on
// arm64 without knowledge of the architecture.
func TestFindLoader(t *testing.T) {
	t.Parallel()

	for _, loader := range []string{"ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"} {
		t.Run(loader, func(t *testing.T) {
			t.Parallel()

			tree := t.TempDir()

			lib := filepath.Join(tree, "lib")
			if err := os.Mkdir(lib, 0o750); err != nil {
				t.Fatal(err)
			}

			for _, f := range []string{loader, "libzfs.so.6", "libc.so.6"} {
				if err := os.WriteFile(filepath.Join(lib, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := findLoader(tree)
			if err != nil {
				t.Fatal(err)
			}

			if filepath.Base(got) != loader {
				t.Errorf("findLoader() = %q, want the %s loader", got, loader)
			}
		})
	}

	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, "lib"), 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := findLoader(tree); err == nil {
		t.Error("a tree with no loader must be an error, and not an empty path")
	}
}
