package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shim selects the bundled OpenZFS userland that runs against the host's
// kernel module. A wrong selection does not fail loudly. It runs a 2.3 zpool
// against a 2.4 module, or a 2.4 zpool against a 2.3 module. The failure then
// appears as an ioctl error, which reads like a ZFS problem and not like a
// parsing problem. These tests therefore pin down the parsing.
func TestDetectVersionFromFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "debian package version", contents: "2.3.2-2\n", want: "2.3"},
		{name: "forky", contents: "2.4.4-1\n", want: "2.4"},
		{name: "no packaging suffix", contents: "2.3.9\n", want: "2.3"},
		{name: "experimental suffix", contents: "2.3.99-1~exp1\n", want: "2.3"},
		{name: "leading and trailing space", contents: "  2.4.0-rc1  \n", want: "2.4"},
		{name: "double digit minor", contents: "2.10.1-1\n", want: "2.10"},
		{name: "empty", contents: "\n", wantErr: true},
		{name: "not a version", contents: "unknown\n", wantErr: true},
		{name: "major only", contents: "2\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "version")
			if err := os.WriteFile(source, []byte(tc.contents), 0o644); err != nil {
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

// A node without the module is the normal case on a mixed cluster, and on
// every CI runner. The shim must report that in terms a reader can act on. It
// must not fail at a deeper level instead.
func TestDetectVersionMissingFile(t *testing.T) {
	_, err := detectVersion(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("want an error when the version file does not exist")
	}
	if !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("error should say the module is not loaded, got: %v", err)
	}
}

func TestDetectVersionEnvOverrides(t *testing.T) {
	// The override is for nodes where /sys is not at the path the shim
	// expects. It also lets a test run a tree other than the host's.
	source := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(source, []byte("2.3.2-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv(versionEnv, "2.4.4-1")
	got, err := detectVersion(source)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2.4" {
		t.Errorf("env override = %q, want 2.4 (the file said 2.3)", got)
	}

	t.Setenv(versionEnv, "nonsense")
	if _, err := detectVersion(source); err == nil {
		t.Error("a malformed override should fail rather than fall through to the file")
	}
}

// The error for an unbundled branch names the branches that the image does
// carry. The list must therefore show the trees that are present.
func TestAvailable(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"2.4", "2.3"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file is not a tree.
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(available(root), ", ")
	if got != "2.3, 2.4" {
		t.Errorf("available() = %q, want \"2.3, 2.4\" (sorted, directories only)", got)
	}

	if got := strings.Join(available(filepath.Join(root, "absent")), ", "); got != "none" {
		t.Errorf("available() on a missing root = %q, want \"none\"", got)
	}
}

// Each tree carries its own loader, and the name of that loader is
// architecture-specific. The glob is what lets the shim run on amd64 and on
// arm64 without knowledge of the architecture.
func TestFindLoader(t *testing.T) {
	for _, loader := range []string{"ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"} {
		t.Run(loader, func(t *testing.T) {
			tree := t.TempDir()
			lib := filepath.Join(tree, "lib")
			if err := os.Mkdir(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{loader, "libzfs.so.6", "libc.so.6"} {
				if err := os.WriteFile(filepath.Join(lib, f), nil, 0o644); err != nil {
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
	if err := os.Mkdir(filepath.Join(tree, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findLoader(tree); err == nil {
		t.Error("a tree with no loader should be an error, not an empty path")
	}
}
