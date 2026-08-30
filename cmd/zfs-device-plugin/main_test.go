package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// The kubelet's own constant for its socket holds a full path, not a file
// name. A join of that constant onto the plugin directory gave
// /var/lib/kubelet/device-plugins/var/lib/kubelet/device-plugins/kubelet.sock
// and registration failed with ENOENT. CI cannot run a registration, because
// that needs a kubelet. But CI can test the path arithmetic, and the bug was
// in the path arithmetic.
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
			// The constant ends with a slash, and a caller can do the same.
			name:        "trailing slash",
			dir:         "/var/lib/kubelet/device-plugins/",
			wantKubelet: "/var/lib/kubelet/device-plugins/kubelet.sock",
			wantPlugin:  "/var/lib/kubelet/device-plugins/zfs-exporter.sock",
		},
		{
			// Some distributions put the kubelet root elsewhere.
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

// fakeKubelet answers the Registration service as the real kubelet does, on a
// socket in a temporary directory. It makes registration testable without a
// node. The code under test needs only a listener at the path that it
// computes, and a wrong path is the bug that this test guards against.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	got chan *pluginapi.RegisterRequest
}

func (f *fakeKubelet) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	f.got <- req
	return &pluginapi.Empty{}, nil
}

// socketDir returns a temporary directory with a path short enough for a unix
// socket. sun_path is 104 bytes on darwin and 108 bytes on linux. On macOS,
// t.TempDir() returns /var/folders/<...>/<TestName><digits>/001, which exceeds
// that limit before the socket name is added.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startFakeKubelet(t *testing.T, dir string) *fakeKubelet {
	t.Helper()

	listener, err := net.Listen("unix", filepath.Join(dir, kubeletSocketName))
	if err != nil {
		t.Fatalf("fake kubelet could not listen: %v", err)
	}

	fake := &fakeKubelet{got: make(chan *pluginapi.RegisterRequest, 1)}
	server := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return fake
}

func TestRegisterReachesTheKubelet(t *testing.T) {
	dir := socketDir(t)
	fake := startFakeKubelet(t, dir)

	cfg := config{
		resourceName: "example.test/dev-zfs",
		devicePath:   "/dev/null",
		pluginDir:    dir,
		count:        1,
	}
	if err := register(context.Background(), cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	select {
	case req := <-fake.got:
		if req.ResourceName != cfg.resourceName {
			t.Errorf("ResourceName = %q, want %q", req.ResourceName, cfg.resourceName)
		}
		if req.Endpoint != socketName {
			t.Errorf("Endpoint = %q, want %q -- the kubelet dials this relative to its own directory", req.Endpoint, socketName)
		}
		if req.Version != pluginapi.Version {
			t.Errorf("Version = %q, want %q", req.Version, pluginapi.Version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the kubelet never received a registration")
	}
}

// Registration against a directory with no kubelet must fail. It must not
// appear to succeed, because the supervisor loop uses the error to retry.
func TestRegisterFailsWithoutKubelet(t *testing.T) {
	cfg := config{resourceName: "example.test/dev-zfs", pluginDir: socketDir(t), count: 1}
	if err := register(context.Background(), cfg); err == nil {
		t.Fatal("register should fail when nothing is listening")
	}
}

// The kubelet acts on the DeviceSpec. Permissions in particular carry weight.
// libzfs opens /dev/zfs with O_RDWR even to list a pool, so "r" here would
// fail at open() on a real node, and at no earlier point.
func TestAllocateReturnsTheDevice(t *testing.T) {
	p := &plugin{cfg: config{devicePath: "/dev/null", count: 1}, ctx: context.Background()}

	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{}, {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ContainerResponses) != 2 {
		t.Fatalf("got %d container responses, want one per request", len(resp.ContainerResponses))
	}

	dev := resp.ContainerResponses[0].Devices[0]
	if dev.HostPath != "/dev/null" || dev.ContainerPath != "/dev/null" {
		t.Errorf("paths = %q -> %q, want the device on both sides", dev.HostPath, dev.ContainerPath)
	}
	if dev.Permissions != "rw" {
		t.Errorf("Permissions = %q, want \"rw\": libzfs opens the device O_RDWR", dev.Permissions)
	}
}

func TestAllocateRefusesWhenTheDeviceIsGone(t *testing.T) {
	p := &plugin{
		cfg: config{devicePath: filepath.Join(t.TempDir(), "absent"), count: 1},
		ctx: context.Background(),
	}
	_, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{}},
	})
	if err == nil {
		t.Fatal("allocating a device that is not there should fail")
	}
}

// present() decides whether a node advertises any device, so it must separate
// a character device from a file that only exists.
func TestPresent(t *testing.T) {
	if !present("/dev/null") {
		t.Error("/dev/null is a character device and should be present")
	}
	regular := filepath.Join(t.TempDir(), "not-a-device")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if present(regular) {
		t.Error("a regular file must not be mistaken for the device")
	}
	if present(filepath.Join(t.TempDir(), "absent")) {
		t.Error("a missing path is not present")
	}
}

// listAndWatchRecorder replaces the kubelet's side of the stream.
type listAndWatchRecorder struct {
	pluginapi.DevicePlugin_ListAndWatchServer
	ctx  context.Context
	sent chan *pluginapi.ListAndWatchResponse
}

func (r *listAndWatchRecorder) Send(resp *pluginapi.ListAndWatchResponse) error {
	r.sent <- resp
	return nil
}

func (r *listAndWatchRecorder) Context() context.Context { return r.ctx }

func TestListAndWatchReportsTheDevice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		count int
		want  int
	}{
		{name: "device present", path: "/dev/null", count: 1, want: 1},
		{name: "count is honoured", path: "/dev/null", count: 3, want: 3},
		{name: "no device on this node", path: "/nonexistent/zfs", count: 1, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			p := &plugin{cfg: config{devicePath: tc.path, count: tc.count}, ctx: ctx}
			rec := &listAndWatchRecorder{ctx: ctx, sent: make(chan *pluginapi.ListAndWatchResponse, 1)}

			done := make(chan error, 1)
			go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, rec) }()

			select {
			case resp := <-rec.sent:
				if len(resp.Devices) != tc.want {
					t.Errorf("reported %d devices, want %d", len(resp.Devices), tc.want)
				}
				for _, d := range resp.Devices {
					if d.Health != pluginapi.Healthy {
						t.Errorf("device %q health = %q, want %q", d.ID, d.Health, pluginapi.Healthy)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ListAndWatch sent nothing")
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("ListAndWatch returned %v, want nil on cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ListAndWatch did not return when the stream was cancelled")
			}
		})
	}
}
