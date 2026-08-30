// Command zfs-device-plugin advertises the node's /dev/zfs to the kubelet as a
// schedulable resource. The kubelet can then give the device to a pod that
// does not run privileged.
//
// A container can open only a device node that the device cgroup permits, and
// Kubernetes does not add hostPath devices to that allowlist. Two in-tree
// solutions exist. The first is `privileged: true`, which grants every
// capability and disables seccomp. Both effects are measured, not assumed. The
// second is a device plugin, which lets the kubelet insert the device into an
// otherwise unprivileged container.
//
// That difference is the reason this plugin exists. OpenZFS gates its mutating
// ioctls on CAP_SYS_ADMIN (secpolicy_sys_config, secpolicy_zfs), and its read
// paths require no privilege (zfs_secpolicy_read). An exporter that holds the
// device with no capabilities can therefore read pool and dataset state, and
// the kernel refuses every operation that would change that state. A
// privileged exporter has no such protection.
//
// This plugin is privileged, because the kubelet's plugin directory requires
// it. Kubernetes documents that requirement, so it is not a choice made here.
// The plugin is not reachable. It serves one gRPC service on a Unix socket in
// that directory, it has no network listener, and it never opens the device
// that it supplies.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tschuering/zfs-exporter-helm/internal/logging"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// By convention, an extended resource name carries a vendor prefix
	// (nvidia.com/gpu, devic.es/dri). That prefix should be a domain that
	// the publisher of the plugin controls. This one is the host that
	// serves the project's Helm repository.
	defaultResourceName = "tschuering.github.io/dev-zfs"
	defaultDevicePath   = "/dev/zfs"
	defaultPluginDir    = pluginapi.DevicePluginPath
	socketName          = "zfs-exporter.sock"

	// This is deliberately not pluginapi.KubeletSocket. That constant holds
	// the full path. A join of that constant onto the plugin directory gives
	// /var/lib/kubelet/device-plugins/var/lib/kubelet/device-plugins/kubelet.sock
	// which is the bug that this constant replaced. The directory must stay
	// configurable, so only the file name belongs here.
	kubeletSocketName = "kubelet.sock"

	// pollInterval is how often the plugin examines the device. watchInterval
	// is how often it examines the kubelet socket for the replacement that a
	// kubelet restart creates.
	pollInterval  = 30 * time.Second
	watchInterval = 5 * time.Second
)

type config struct {
	resourceName string
	devicePath   string
	pluginDir    string
	count        int
}

func main() {
	// Set this before any code that can log. The configuration read below can
	// warn about a malformed value, and that warning must use the same format
	// as every line after it.
	slog.SetDefault(logging.New(os.Stderr))

	cfg := config{
		resourceName: env("RESOURCE_NAME", defaultResourceName),
		devicePath:   env("DEVICE_PATH", defaultDevicePath),
		pluginDir:    env("PLUGIN_DIR", defaultPluginDir),
		count:        envInt("DEVICE_COUNT", 1),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Advertising device",
		"resource", cfg.resourceName, "device", cfg.devicePath, "count", cfg.count)

	// Registration is not permanent. A kubelet restart clears the plugin
	// directory, and every plugin must register again. serve() returns after a
	// kubelet restart, and on any error. The loop then repeats.
	for ctx.Err() == nil {
		if err := serve(ctx, cfg); err != nil && ctx.Err() == nil {
			slog.Error("Restarting after failure", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(watchInterval):
			}
		}
	}
	slog.Info("Shutting down")
}

// serve publishes the plugin socket and registers with the kubelet. It then
// blocks until the context ends, or until something replaces the kubelet
// socket.
func serve(ctx context.Context, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	socket := pluginSocketPath(cfg.pluginDir)
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer listener.Close()

	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, &plugin{cfg: cfg, ctx: ctx})

	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()
	defer server.Stop()

	if err := register(ctx, cfg); err != nil {
		return fmt.Errorf("registering with kubelet: %w", err)
	}
	slog.Info("Registered with kubelet", "resource", cfg.resourceName)

	// A kubelet restart creates a new kubelet.sock. A check of the file
	// identity detects that, and it needs no filesystem-notification
	// dependency.
	kubeletSocket := kubeletSocketPath(cfg.pluginDir)
	id, err := identity(kubeletSocket)
	if err != nil {
		return fmt.Errorf("stat %s: %w", kubeletSocket, err)
	}

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return fmt.Errorf("grpc server stopped: %w", err)
		case <-ticker.C:
			current, err := identity(kubeletSocket)
			if err != nil {
				return fmt.Errorf("kubelet socket gone: %w", err)
			}
			if current != id {
				return errors.New("kubelet restarted")
			}
		}
	}
}

// register gives the kubelet the socket to call back on, and the resource
// name to advertise.
func register(ctx context.Context, cfg config) error {
	target := "unix://" + kubeletSocketPath(cfg.pluginDir)

	// This is a Unix socket on the same filesystem. There is no transport
	// security to negotiate, and nothing else can reach the socket.
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = pluginapi.NewRegistrationClient(conn).Register(call, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     socketName,
		ResourceName: cfg.resourceName,
	})
	return err
}

type plugin struct {
	pluginapi.UnimplementedDevicePluginServer
	cfg config
	ctx context.Context
}

func (p *plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch reports the device while the kubelet listens. It sends an
// update each time the device appears or disappears. A node without ZFS
// reports zero devices, which stops the scheduler from placing the exporter on
// that node.
func (p *plugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	last := -1
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		healthy := 0
		if present(p.cfg.devicePath) {
			healthy = p.cfg.count
		}
		if healthy != last {
			if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: devices(healthy)}); err != nil {
				return err
			}
			slog.Info("Reporting devices", "count", healthy)
			last = healthy
		}

		select {
		case <-p.ctx.Done():
			return nil
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Allocate gives the kubelet the device node to insert. This is the only
// privileged operation in the design, and the kubelet performs it. The plugin
// never opens the device itself.
func (p *plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if !present(p.cfg.devicePath) {
		return nil, fmt.Errorf("%s is not present on this node", p.cfg.devicePath)
	}

	resp := &pluginapi.AllocateResponse{}
	for range req.GetContainerRequests() {
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: []*pluginapi.DeviceSpec{{
				HostPath:      p.cfg.devicePath,
				ContainerPath: p.cfg.devicePath,
				// libzfs opens /dev/zfs with O_RDWR even to list a pool, so
				// a read-only value here would fail at open(). The absence of
				// CAP_SYS_ADMIN stops the container from changing anything.
				// OpenZFS checks that capability on each ioctl. The device
				// mode does not control this.
				Permissions: "rw",
			}},
		})
	}
	return resp, nil
}

func (p *plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func devices(n int) []*pluginapi.Device {
	out := make([]*pluginapi.Device, 0, n)
	for i := range n {
		out = append(out, &pluginapi.Device{
			ID:     fmt.Sprintf("dev-zfs-%d", i),
			Health: pluginapi.Healthy,
		})
	}
	return out
}

// kubeletSocketPath returns the path where the kubelet listens for
// registrations. pluginSocketPath returns the path where this plugin listens
// for calls from the kubelet. Both paths are relative to the plugin directory,
// which moves on distributions that put the kubelet root elsewhere.
func kubeletSocketPath(pluginDir string) string {
	return filepath.Join(pluginDir, kubeletSocketName)
}

func pluginSocketPath(pluginDir string) string {
	return filepath.Join(pluginDir, socketName)
}

func present(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// identity returns a value that changes when something replaces the file. A
// kubelet restart replaces the kubelet socket in this way.
func identity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime().String(), nil
	}
	return fmt.Sprintf("%d:%d", sys.Dev, sys.Ino), nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		slog.Warn("Ignoring malformed value, want a positive integer",
			"variable", key, "value", v)
		return fallback
	}
	return n
}
