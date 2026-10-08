package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Machine holds metadata about a Firecracker machine and its associated resources.
// It is the Firecracker implementation of VM.
type Machine struct {
	*firecracker.Machine

	Name      string
	RunnerID  int64
	Pool      string
	CreatedAt time.Time

	vsockCID    uint32
	vsockPath   string
	leaseCancel func(context.Context) error // containerd lease cancel function
	vmmCtx      context.Context
	vmmCancel   context.CancelFunc
	done        chan struct{}
}

var _ VM = (*Machine)(nil)

func (m *Machine) ConnectToGuestAgent(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return vsock.DialContext(ctx, m.vsockPath, 9001)
	}

	// Create gRPC client with VSOCK transport
	// Use "passthrough:" resolver to bypass name resolution and pass directly to dialer
	conn, err := grpc.NewClient(
		"passthrough:vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("grpc dial: %w", err)
	}

	client := agentv1.NewAgentServiceClient(conn)
	return conn, client, nil
}

func (m *Machine) GetAddr() string {
	addr := ""
	if len(m.Cfg.NetworkInterfaces) > 0 && m.Cfg.NetworkInterfaces[0].StaticConfiguration != nil &&
		m.Cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration != nil {
		addr = m.Cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration.IPAddr.IP.String()
	}

	return addr
}

// Info implements VM.
func (m *Machine) Info() VMInfo {
	return VMInfo{Name: m.Name, Pool: m.Pool, Addr: m.GetAddr(), CreatedAt: m.CreatedAt}
}

// RunnerState implements VM.
func (m *Machine) RunnerState(ctx context.Context) (string, error) {
	conn, client, err := m.ConnectToGuestAgent(ctx)
	if err != nil {
		return RunnerStateUnknown, err
	}
	defer conn.Close()

	resp, err := client.GetRunnerState(ctx, &agentv1.GetRunnerStateRequest{})
	if err != nil {
		return RunnerStateUnknown, err
	}

	return resp.GetState(), nil
}

// RunnerVersion implements VM.
func (m *Machine) RunnerVersion(ctx context.Context) (string, error) {
	conn, client, err := m.ConnectToGuestAgent(ctx)
	if err != nil {
		return "Unknown", err
	}
	defer conn.Close()

	resp, err := client.GetRunnerVersion(ctx, &agentv1.GetRunnerVersionRequest{})
	if err != nil {
		return "Unknown", err
	}

	return resp.GetVersion(), nil
}

// Stop implements VM.
func (m *Machine) Stop(_ context.Context) error {
	return m.StopVMM()
}

// Kill implements VM.
func (m *Machine) Kill() error {
	return m.StopVMM()
}

// Done implements VM.
func (m *Machine) Done() <-chan struct{} {
	return m.done
}
