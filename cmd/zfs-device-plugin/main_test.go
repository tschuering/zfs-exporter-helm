package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// devNull stands in for /dev/zfs. It is a character device on every system
// that runs these tests.
const devNull = "/dev/null"

// The kubelet's own constant for its socket holds a full path, not a file
// name. A join of that constant onto the plugin directory gave
// /var/lib/kubelet/device-plugins/var/lib/kubelet/device-plugins/kubelet.sock
// and registration failed with ENOENT. CI cannot run a registration, because
// that needs a kubelet. But CI can test the path arithmetic, and the bug was
// in the path arithmetic.
func TestSocketPaths(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

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

	//nolint:usetesting // the path of t.TempDir() exceeds the sun_path limit; see above
	dir, err := os.MkdirTemp("/tmp", "zdp")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

func startFakeKubelet(t *testing.T, dir string) *fakeKubelet {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, kubeletSocketName))
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
	t.Parallel()

	dir := socketDir(t)
	fake := startFakeKubelet(t, dir)

	cfg := config{
		resourceName: "example.test/dev-zfs",
		devicePath:   devNull,
		pluginDir:    dir,
		count:        1,
	}
	if err := register(t.Context(), cfg); err != nil {
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
	t.Parallel()

	cfg := config{resourceName: "example.test/dev-zfs", pluginDir: socketDir(t), count: 1}
	if err := register(t.Context(), cfg); err == nil {
		t.Fatal("register must fail when nothing listens")
	}
}

// serveConfig returns a configuration that points serve() at dir. The watch
// interval is short, so a test sees a socket change without a long wait.
func serveConfig(dir string) config {
	return config{
		resourceName:  "example.test/dev-zfs",
		devicePath:    devNull,
		pluginDir:     dir,
		count:         1,
		watchInterval: 20 * time.Millisecond,
	}
}

// startServe runs serve() against a fake kubelet in dir, and returns after
// the registration arrived. From that point the caller can change the kubelet
// socket, and read the return value of serve() from the channel. A stale file
// sits at the plugin socket path, so every use also proves that serve()
// clears the leftover of a crash.
func startServe(t *testing.T, dir string) (<-chan error, context.CancelFunc) {
	t.Helper()

	fake := startFakeKubelet(t, dir)

	if err := os.WriteFile(pluginSocketPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- serve(ctx, serveConfig(dir)) }()

	select {
	case <-fake.got:
	case <-time.After(5 * time.Second):
		t.Fatal("serve() never registered with the fake kubelet")
	}

	// The registration arrived at the fake kubelet, but serve() itself still
	// sits in its setup: the register call must return, and the first look
	// at the kubelet socket must record the socket's identity. A change
	// before that point trips the setup errors instead of the watch loop.
	// Five quiet intervals let serve() reach the loop.
	time.Sleep(5 * serveConfig(dir).watchInterval)

	return done, cancel
}

// When the context ends, serve() must return nil promptly. The supervisor
// loop in main() reads that value as a clean shutdown.
func TestServeStopsOnCancel(t *testing.T) {
	t.Parallel()

	done, cancel := startServe(t, socketDir(t))

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve() = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after the context ended")
	}
}

// A kubelet restart replaces kubelet.sock, and serve() must return so that
// the supervisor registers again. The rename below replaces the path in one
// step. Thus no check can find the path absent, and the test cannot drift
// into the socket-gone error instead.
func TestServeDetectsKubeletRestart(t *testing.T) {
	t.Parallel()

	dir := socketDir(t)
	done, _ := startServe(t, dir)

	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(replacement, kubeletSocketPath(dir)); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "kubelet restarted") {
			t.Errorf("serve() = %v, want the kubelet-restart error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not notice the replaced kubelet socket")
	}
}

// A kubelet socket that disappears without a replacement is a different
// condition than a restart, and it has its own message.
func TestServeFailsWhenKubeletSocketGoes(t *testing.T) {
	t.Parallel()

	dir := socketDir(t)
	done, _ := startServe(t, dir)

	if err := os.Remove(kubeletSocketPath(dir)); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "kubelet socket gone") {
			t.Errorf("serve() = %v, want the socket-gone error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not notice the missing kubelet socket")
	}
}

// Without a kubelet, serve() must return the registration error. The
// supervisor loop retries on it, so a silent nil here disables the plugin.
func TestServeFailsWithoutKubelet(t *testing.T) {
	t.Parallel()

	err := serve(t.Context(), serveConfig(socketDir(t)))
	if err == nil || !strings.Contains(err.Error(), "registering") {
		t.Errorf("serve() = %v, want a registration error", err)
	}
}

// The kubelet acts on the DeviceSpec. Permissions in particular carry weight.
// libzfs opens /dev/zfs with O_RDWR even to list a pool, so "r" here would
// fail at open() on a real node, and at no earlier point.
func TestAllocateReturnsTheDevice(t *testing.T) {
	t.Parallel()

	p := &plugin{cfg: config{devicePath: devNull, count: 1}, ctx: t.Context()}

	resp, err := p.Allocate(t.Context(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{}, {}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.ContainerResponses) != 2 {
		t.Fatalf("got %d container responses, want one per request", len(resp.ContainerResponses))
	}

	dev := resp.ContainerResponses[0].Devices[0]
	if dev.HostPath != devNull || dev.ContainerPath != devNull {
		t.Errorf("paths = %q -> %q, want the device on both sides", dev.HostPath, dev.ContainerPath)
	}

	if dev.Permissions != "rw" {
		t.Errorf("Permissions = %q, want \"rw\": libzfs opens the device O_RDWR", dev.Permissions)
	}
}

func TestAllocateRefusesWhenTheDeviceIsGone(t *testing.T) {
	t.Parallel()

	p := &plugin{
		cfg: config{devicePath: filepath.Join(t.TempDir(), "absent"), count: 1},
		ctx: t.Context(),
	}

	_, err := p.Allocate(t.Context(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{}},
	})
	if err == nil {
		t.Fatal("Allocate must fail when the device is not there")
	}
	// The kubelet acts on the status code. A bare error arrives as Unknown.
	// That code says nothing about whether another attempt is worthwhile. The
	// device appears as soon as the module loads, so this condition is
	// Unavailable.
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status code = %s, want %s", got, codes.Unavailable)
	}
}

// identity() must stay stable while the file stays, and it must change when
// something replaces the file. That change is how serve() notices a kubelet
// restart.
func TestIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := filepath.Join(dir, "sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}

	if again, err := identity(path); err != nil || again != first {
		t.Errorf("identity() = %q, %v on the unchanged file, want %q again", again, err, first)
	}

	// The replacement exists next to the original, so the two cannot share
	// an inode. The rename then moves it over the original in one step.
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}

	if second, err := identity(path); err != nil || second == first {
		t.Errorf("identity() = %q, %v after the replacement, want a new value", second, err)
	}

	if _, err := identity(filepath.Join(dir, "absent")); err == nil {
		t.Error("identity() on a missing path must be an error")
	}
}

// env() must treat an empty value as unset. The chart renders an empty
// string for a value that nobody set, and the fallback must apply then.
func TestEnv(t *testing.T) {
	t.Setenv("ZDP_TEST_VALUE", "from-env")

	if got := env("ZDP_TEST_VALUE", "fallback"); got != "from-env" {
		t.Errorf("env() = %q, want the set value", got)
	}

	t.Setenv("ZDP_TEST_VALUE", "")

	if got := env("ZDP_TEST_VALUE", "fallback"); got != "fallback" {
		t.Errorf("env() = %q, want the fallback for an empty value", got)
	}
}

// DEVICE_COUNT comes from the chart, so a mistyped value reaches envInt.
// The comment in envInt tells why a partial parse is worse than the
// fallback. This test makes sure that each malformed or out-of-range value
// falls back.
func TestEnvInt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "empty", value: "", want: 1},
		{name: "plain integer", value: "3", want: 3},
		{name: "at the upper bound", value: "1024", want: 1024},
		{name: "trailing letters", value: "12abc", want: 1},
		{name: "decimal", value: "2.9", want: 1},
		{name: "two numbers", value: "3 4", want: 1},
		{name: "surrounding space", value: "  5  ", want: 1},
		{name: "zero", value: "0", want: 1},
		{name: "negative", value: "-2", want: 1},
		{name: "above the upper bound", value: "1025", want: 1},
		{name: "far above the upper bound", value: "1000000000", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DEVICE_COUNT", tc.value)

			if got := envInt("DEVICE_COUNT", 1); got != tc.want {
				t.Errorf("envInt(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// Both methods below are part of the device-plugin API. The kubelet can call
// them, so each must answer with an empty message and no error.
func TestUnusedHandlersAnswer(t *testing.T) {
	t.Parallel()

	p := &plugin{}

	opts, err := p.GetDevicePluginOptions(t.Context(), &pluginapi.Empty{})
	if err != nil || opts == nil {
		t.Errorf("GetDevicePluginOptions() = %v, %v, want an empty answer", opts, err)
	}

	resp, err := p.PreStartContainer(t.Context(), &pluginapi.PreStartContainerRequest{})
	if err != nil || resp == nil {
		t.Errorf("PreStartContainer() = %v, %v, want an empty answer", resp, err)
	}
}

// present() decides whether a node advertises any device, so it must separate
// a character device from a file that only exists.
func TestPresent(t *testing.T) {
	t.Parallel()

	if !present(devNull) {
		t.Error("/dev/null is a character device and must be present")
	}

	regular := filepath.Join(t.TempDir(), "not-a-device")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if present(regular) {
		t.Error("a regular file must not be mistaken for the device")
	}

	if present(filepath.Join(t.TempDir(), "absent")) {
		t.Error("a missing path is not present")
	}
}

// listAndWatchRecorder replaces the kubelet's side of the stream. When
// sendErr is set, every Send fails with it, like on a stream whose far side
// went away.
type listAndWatchRecorder struct {
	pluginapi.DevicePlugin_ListAndWatchServer
	// The stream interface has a Context() method, so the fake must hold the
	// context that this method returns.
	//nolint:containedctx // the field backs the Context() method of the stream interface
	ctx     context.Context
	sent    chan *pluginapi.ListAndWatchResponse
	sendErr error
}

func (r *listAndWatchRecorder) Send(resp *pluginapi.ListAndWatchResponse) error {
	if r.sendErr != nil {
		return r.sendErr
	}

	r.sent <- resp

	return nil
}

func (r *listAndWatchRecorder) Context() context.Context { return r.ctx }

func TestListAndWatchReportsTheDevice(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		path  string
		count int
		want  int
	}{
		{name: "device present", path: devNull, count: 1, want: 1},
		{name: "count is honoured", path: devNull, count: 3, want: 3},
		{name: "no device on this node", path: "/nonexistent/zfs", count: 1, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
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
				t.Fatal("ListAndWatch did not return when the stream was canceled")
			}
		})
	}
}

// When Send fails, the stream is dead. ListAndWatch must return the error,
// so that gRPC closes the call and the kubelet opens a new one. A loop that
// continues on a dead stream reports devices to nobody.
func TestListAndWatchStopsOnSendError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the stream is gone")

	p := &plugin{cfg: config{devicePath: devNull, count: 1}, ctx: t.Context()}
	rec := &listAndWatchRecorder{ctx: t.Context(), sendErr: sentinel}

	if err := p.ListAndWatch(&pluginapi.Empty{}, rec); !errors.Is(err, sentinel) {
		t.Errorf("ListAndWatch() = %v, want the error of the failed Send", err)
	}
}
