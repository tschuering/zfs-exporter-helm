package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shim decides which bundled OpenZFS userland runs against the host's
// kernel module. Getting that wrong does not fail loudly -- it runs a 2.3
// zpool against a 2.4 module, or the other way round, and the failure surfaces
// as an ioctl error that reads like a ZFS problem rather than a parsing one.
// So the parsing is worth pinning down.
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

// A node without the module is the ordinary case on a mixed cluster, and on
// every CI runner. It has to say so in terms someone can act on, rather than
// failing somewhere deeper.
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
	// The override exists for nodes where /sys is not where the shim expects,
	// and for exercising a tree that is not the host's.
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

// The error a user sees when their branch is not bundled names what is, so
// listing has to reflect the trees actually present.
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

// Each tree carries its own loader, whose name is architecture-specific. The
// glob is what makes the shim work on both amd64 and arm64 without knowing
// which it is on.
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
