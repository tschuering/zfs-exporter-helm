package main

import "testing"

// The kubelet's own constant for its socket is a full path, not a file name,
// so joining it onto the plugin directory produced
// /var/lib/kubelet/device-plugins/var/lib/kubelet/device-plugins/kubelet.sock
// and registration failed with ENOENT. Nothing in CI can exercise
// registration -- it needs a kubelet -- but the path arithmetic is testable,
// and that is where the bug was.
func TestSocketPaths(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dir         string
		wantKubelet string
		wantPlugin  string
	}{
		{
			name:        "default directory",
			dir:         "/var/lib/kubelet/device-plugins",
			wantKubelet: "/var/lib/kubelet/device-plugins/kubelet.sock",
			wantPlugin:  "/var/lib/kubelet/device-plugins/zfs-exporter.sock",
		},
		{
			// The constant carries one, and callers may too.
			name:        "trailing slash",
			dir:         "/var/lib/kubelet/device-plugins/",
			wantKubelet: "/var/lib/kubelet/device-plugins/kubelet.sock",
			wantPlugin:  "/var/lib/kubelet/device-plugins/zfs-exporter.sock",
		},
		{
			// Distributions that site the kubelet root elsewhere.
			name:        "custom kubelet root",
			dir:         "/var/lib/k0s/kubelet/device-plugins",
			wantKubelet: "/var/lib/k0s/kubelet/device-plugins/kubelet.sock",
			wantPlugin:  "/var/lib/k0s/kubelet/device-plugins/zfs-exporter.sock",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kubeletSocketPath(tc.dir); got != tc.wantKubelet {
				t.Errorf("kubeletSocketPath(%q) = %q, want %q", tc.dir, got, tc.wantKubelet)
			}
			if got := pluginSocketPath(tc.dir); got != tc.wantPlugin {
				t.Errorf("pluginSocketPath(%q) = %q, want %q", tc.dir, got, tc.wantPlugin)
			}
		})
	}
}
