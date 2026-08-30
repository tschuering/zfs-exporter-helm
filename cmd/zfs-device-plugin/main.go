// Command zfs-device-plugin advertises the node's /dev/zfs to the kubelet as a
// schedulable resource, so a pod can be granted the device without running
// privileged.
//
// Why this exists at all: a container may only open a device node the device
// cgroup permits, and Kubernetes does not add hostPath devices to that
// allowlist. The only in-tree ways through are `privileged: true` -- which
// grants every capability and disables seccomp, both measured, not assumed --
// or a device plugin, which lets the kubelet inject the device into an
// otherwise unprivileged container.
//
// That distinction is the whole point. OpenZFS gates its mutating ioctls on
// CAP_SYS_ADMIN (secpolicy_sys_config, secpolicy_zfs) while its read paths
// require nothing (zfs_secpolicy_read). So an exporter holding the device with
// no capabilities can read pool and dataset state, and the kernel refuses
// anything that would change it. A privileged exporter has no such protection.
//
// This plugin is privileged, because the kubelet's plugin directory demands it
// -- that is documented, not a choice. What it is not: reachable. It serves one
// gRPC service on a Unix socket in that directory, has no network listener, and
// never opens the device it hands out.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// Extended resource names are vendor-qualified by convention
	// (nvidia.com/gpu, devic.es/dri), so the prefix should be a domain
	// whoever ships the plugin actually controls. This one is the host
	// serving the project's Helm repository.
	defaultResourceName = "tschuering.github.io/dev-zfs"
	defaultDevicePath   = "/dev/zfs"
	defaultPluginDir    = pluginapi.DevicePluginPath
	socketName          = "zfs-exporter.sock"

	// How often the device is re-checked, and how often the kubelet socket is
	// examined for the replacement that a kubelet restart produces.
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
	cfg := config{
		resourceName: env("RESOURCE_NAME", defaultResourceName),
		devicePath:   env("DEVICE_PATH", defaultDevicePath),
		pluginDir:    env("PLUGIN_DIR", defaultPluginDir),
		count:        envInt("DEVICE_COUNT", 1),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logf("advertising %s from %s (count %d)", cfg.resourceName, cfg.devicePath, cfg.count)

	// Registration is not once-and-for-all: a kubelet restart wipes the plugin
	// directory and every plugin has to come back. serve() returns when that
	// happens, or on any error, and the loop starts over.
	for ctx.Err() == nil {
		if err := serve(ctx, cfg); err != nil && ctx.Err() == nil {
			logf("restarting: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(watchInterval):
			}
		}
	}
	logf("shutting down")
}

// serve publishes the plugin socket, registers with the kubelet, and blocks
// until the context ends or the kubelet socket is replaced.
func serve(ctx context.Context, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	socket := filepath.Join(cfg.pluginDir, socketName)
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
	logf("registered %s", cfg.resourceName)

	// A kubelet restart recreates kubelet.sock. Watching its identity is
	// enough to notice, and needs no filesystem-notification dependency.
	kubeletSocket := filepath.Join(cfg.pluginDir, pluginapi.KubeletSocket)
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

// register tells the kubelet the socket to call back on and the resource name
// to advertise.
func register(ctx context.Context, cfg config) error {
	target := "unix://" + filepath.Join(cfg.pluginDir, pluginapi.KubeletSocket)

	// A Unix socket on the same filesystem: no transport security to
	// negotiate, and nothing else can reach it.
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

// ListAndWatch reports the device for as long as the kubelet is listening,
// pushing an update whenever the device appears or disappears. A node without
// ZFS reports zero, which is what keeps the exporter from being scheduled
// there at all.
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
			logf("reporting %d device(s)", healthy)
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

// Allocate hands the kubelet the device node to inject. This is the only
// privileged act in the whole design, and it is the kubelet that performs it --
// the plugin never opens the device itself.
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
				// libzfs opens /dev/zfs O_RDWR even to list a pool, so read-only
				// here would fail at open(). What keeps the container from
				// changing anything is the absence of CAP_SYS_ADMIN, which
				// OpenZFS checks per ioctl -- not the device mode.
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

func present(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// identity returns something that changes when a file is replaced, which is
// what a kubelet restart does to its socket.
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
		logf("ignoring %s=%q: want a positive integer", key, v)
		return fallback
	}
	return n
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zfs-device-plugin: "+format+"\n", args...)
}
