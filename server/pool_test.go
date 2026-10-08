package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeVM is a VM whose runner state is set by the test.
type fakeVM struct {
	name string

	mu      sync.Mutex
	state   string
	stopped bool
	done    chan struct{}
}

func (v *fakeVM) Info() VMInfo { return VMInfo{Name: v.name, Pool: "test"} }

func (v *fakeVM) RunnerState(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state, nil
}

func (v *fakeVM) RunnerVersion(context.Context) (string, error) { return "2.338.0", nil }

func (v *fakeVM) ConnectToGuestAgent(context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	return nil, nil, errors.New("fake VM has no agent")
}

func (v *fakeVM) Stop(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopLocked()
}

func (v *fakeVM) Kill() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopLocked()
}

func (v *fakeVM) stopLocked() error {
	if !v.stopped {
		v.stopped = true
		close(v.done)
	}
	return nil
}

func (v *fakeVM) Done() <-chan struct{} { return v.done }

func (v *fakeVM) setState(state string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state = state
}

// exit simulates the runner finishing its job and the VM shutting down.
func (v *fakeVM) exit() { _ = v.Kill() }

func (v *fakeVM) isStopped() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopped
}

// fakeBackend creates fakeVMs, all starting in the given state.
type fakeBackend struct {
	mu       sync.Mutex
	created  []*fakeVM
	failNext int
	state    string
}

func (b *fakeBackend) CreateVM(context.Context) (VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.failNext > 0 {
		b.failNext--
		return nil, errors.New("boom")
	}

	state := b.state
	if state == "" {
		state = RunnerStateIdle
	}

	vm := &fakeVM{name: fmt.Sprintf("vm-%d", len(b.created)), state: state, done: make(chan struct{})}
	b.created = append(b.created, vm)
	return vm, nil
}

func (b *fakeBackend) vms() []*fakeVM {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fakeVM(nil), b.created...)
}

// running returns the names of VMs that haven't been stopped.
func (b *fakeBackend) running() []string {
	var names []string
	for _, vm := range b.vms() {
		if !vm.isStopped() {
			names = append(names, vm.name)
		}
	}
	sort.Strings(names)
	return names
}

func (b *fakeBackend) stopped() []string {
	var names []string
	for _, vm := range b.vms() {
		if vm.isStopped() {
			names = append(names, vm.name)
		}
	}
	sort.Strings(names)
	return names
}

func newTestPool(t *testing.T, backend VMBackend, demand DemandSource) *Pool {
	t.Helper()

	logger := zerolog.Nop()
	p, err := NewPool(&logger, &PoolConfig{Name: "test", Runner: &RunnerConfig{Organization: "org"}}, backend, demand)
	require.NoError(t, err)
	t.Cleanup(p.cancel)
	return p
}

// settle waits for exited VMs to leave the pool, runs one reconcile step and
// waits for VM creation to finish.
func settle(t *testing.T, p *Pool, b *fakeBackend) {
	t.Helper()

	waitForExits(t, p, b)
	p.reconcile()
	p.creates.Wait()
	waitForExits(t, p, b)
}

func waitForExits(t *testing.T, p *Pool, b *fakeBackend) {
	t.Helper()

	require.Eventually(t, func() bool { return len(p.listVMs()) == len(b.running()) },
		time.Second, time.Millisecond, "exited VMs never left the pool")
}

func TestPool_FixedDemandScalesUpToReplicas(t *testing.T) {
	b := &fakeBackend{}
	p := newTestPool(t, b, newFixedDemand(3))

	settle(t, p, b)

	assert.Equal(t, []string{"vm-0", "vm-1", "vm-2"}, b.running())
	assert.Equal(t, 3, p.GetCurrentSize())

	// A second pass changes nothing.
	settle(t, p, b)
	assert.Len(t, b.vms(), 3)
}

func TestPool_FixedDemandReplacesExitedVMs(t *testing.T) {
	b := &fakeBackend{}
	p := newTestPool(t, b, newFixedDemand(2))
	settle(t, p, b)

	// The ephemeral runner on vm-0 finishes its job and the VM exits.
	b.vms()[0].exit()
	settle(t, p, b)

	assert.Equal(t, []string{"vm-1", "vm-2"}, b.running())
}

func TestPool_FixedDemandScaleDownStopsSurplus(t *testing.T) {
	b := &fakeBackend{}
	demand := newFixedDemand(3)
	p := newTestPool(t, b, demand)
	settle(t, p, b)

	require.NoError(t, p.SetReplicas(1))
	settle(t, p, b)

	assert.Len(t, b.running(), 1)
	assert.Len(t, b.stopped(), 2)
	assert.Len(t, b.vms(), 3, "scale-down must not create VMs")
}

func TestPool_RetriesFailedCreates(t *testing.T) {
	b := &fakeBackend{failNext: 1}
	p := newTestPool(t, b, newFixedDemand(2))

	settle(t, p, b)
	assert.Len(t, b.running(), 1)

	settle(t, p, b)
	assert.Len(t, b.running(), 2)
}

func TestPool_SetReplicasOnlyForFixedDemand(t *testing.T) {
	p := newTestPool(t, &fakeBackend{}, demandFunc(func() int { return 1 }))

	assert.Error(t, p.SetReplicas(3))
}

// demandFunc adapts a function to DemandSource.
type demandFunc func() int

func (f demandFunc) Desired(context.Context) int { return f() }
