package server

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
)

// The pool's reconcile loop only talks to the interfaces in this file, so it can
// be driven in tests without Firecracker, containerd or GitHub.

// ErrRunnerBusy is returned by VM.Stop when the runner has picked up a job and
// must be left to finish it.
var ErrRunnerBusy = errors.New("runner is busy")

// Runner states reported by the in-VM agent (see agent/runner).
const (
	RunnerStateStarting  = "Starting"
	RunnerStateIdle      = "Idle"
	RunnerStateRunning   = "Running"
	RunnerStateCompleted = "Completed"
	RunnerStateUnknown   = "Unknown"
)

// VMInfo is static information about a VM.
type VMInfo struct {
	Name      string
	Pool      string
	Addr      string
	CreatedAt time.Time
}

// VM is one runner VM as seen by its pool.
type VM interface {
	Info() VMInfo
	// RunnerState returns the runner state reported by the in-VM agent.
	RunnerState(ctx context.Context) (string, error)
	// RunnerVersion returns the runner version reported by the in-VM agent.
	RunnerVersion(ctx context.Context) (string, error)
	// ConnectToGuestAgent opens a gRPC connection to the in-VM agent.
	ConnectToGuestAgent(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error)
	// Stop stops an idle VM for scale-down. It returns ErrRunnerBusy, without
	// stopping anything, if the runner is running a job.
	Stop(ctx context.Context) error
	// Kill stops the VM unconditionally (server shutdown).
	Kill() error
	// Done is closed once the VM has exited and its resources are released.
	Done() <-chan struct{}
}

// VMBackend creates VMs for a pool.
type VMBackend interface {
	// CreateVM boots one VM. ctx bounds the VM's lifetime: when it is
	// cancelled the VM is torn down.
	CreateVM(ctx context.Context) (VM, error)
}

// DemandSource tells a pool how many VMs it should have.
type DemandSource interface {
	Desired(ctx context.Context) int
}

// runnableDemand is a DemandSource with a background loop, run for the
// lifetime of the pool.
type runnableDemand interface {
	Run(ctx context.Context)
}

// busyReporter is a DemandSource that knows which runners have a job. The pool
// never scales those VMs down.
type busyReporter interface {
	IsBusy(vmName string) bool
}

// fixedDemand is the classic `replicas` behaviour: a constant, operator-set count.
type fixedDemand struct {
	replicas atomic.Int32
}

func newFixedDemand(replicas int) *fixedDemand {
	d := &fixedDemand{}
	d.Set(replicas)
	return d
}

func (d *fixedDemand) Desired(context.Context) int { return int(d.replicas.Load()) }

func (d *fixedDemand) Set(replicas int) { d.replicas.Store(int32(replicas)) }
