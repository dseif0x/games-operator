package devices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Plugin serves the kubelet device plugin API for Resource. Every
// "device" is the same thing (the CDI device), so the count only bounds
// how many containers on the node can hold one at a time.
type Plugin struct {
	// SocketDir is the kubelet's device plugin directory.
	SocketDir string
	// Count is how many devices are advertised.
	Count int
	Log   *slog.Logger

	pluginapi.UnimplementedDevicePluginServer
}

const socketName = "games-operator-input.sock"

// Run serves until ctx ends or the kubelet restarts (its directory is
// recreated and the socket vanishes), which is reported as an error so
// the pod restarts and registers again.
func (p *Plugin) Run(ctx context.Context) error {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Count <= 0 {
		p.Count = 64
	}
	socket := filepath.Join(p.SocketDir, socketName)
	_ = os.Remove(socket)
	lis, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}
	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	defer srv.Stop()

	if err := p.register(ctx); err != nil {
		return err
	}
	p.Log.Info("device plugin registered", "resource", Resource, "devices", p.Count, "cdi", QualifiedName())
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return fmt.Errorf("grpc: %w", err)
		case <-t.C:
			if _, err := os.Stat(socket); err != nil {
				return errors.New("plugin socket vanished (kubelet restarted?)")
			}
		}
	}
}

func (p *Plugin) register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+filepath.Join(p.SocketDir, pluginapi.KubeletSocket), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("kubelet socket: %w", err)
	}
	defer conn.Close()
	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version: pluginapi.Version, Endpoint: socketName, ResourceName: Resource,
	})
	if err != nil {
		return fmt.Errorf("register with the kubelet: %w", err)
	}
	return nil
}

// GetDevicePluginOptions implements the API.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch sends the device list once and keeps the stream open.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	devs := make([]*pluginapi.Device, 0, p.Count)
	for i := 0; i < p.Count; i++ {
		devs = append(devs, &pluginapi.Device{ID: fmt.Sprintf("input-%d", i), Health: pluginapi.Healthy})
	}
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: devs}); err != nil {
		return err
	}
	<-s.Context().Done()
	return nil
}

// Allocate answers every container with the CDI device, as the CRI field
// and as the annotation.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	out := &pluginapi.AllocateResponse{}
	for range req.ContainerRequests {
		out.ContainerResponses = append(out.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			CdiDevices:  []*pluginapi.CDIDevice{{Name: QualifiedName()}},
			Annotations: map[string]string{Annotation: QualifiedName()},
		})
	}
	return out, nil
}

// PreStartContainer implements the API.
func (p *Plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation implements the API.
func (p *Plugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}
