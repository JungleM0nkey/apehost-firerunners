package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Pool represents a pool of Firecracker VMs that are used to run GitHub Actions jobs.
//
// The pool owns the reconcile loop: it compares the desired VM count from its
// DemandSource with the VMs it has and asks its VMBackend to create or stop VMs.
type Pool struct {
	config  *PoolConfig
	backend VMBackend
	demand  DemandSource
	logger  *zerolog.Logger

	vmsMu sync.Mutex
	vms   map[string]*poolVM

	pendingCreates atomic.Int32
	desired        atomic.Int32 // last desired count, for reporting
	active         atomic.Bool

	reconcileMu  sync.Mutex
	creates      sync.WaitGroup
	scaleTrigger chan struct{}
	stopCh       chan struct{}
	doneCh       chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
}

type poolVM struct {
	vm       VM
	stopping bool // Stop was called; don't count it towards the pool size
}

// PoolConfig represents the configuration of a Pool.
type PoolConfig struct {
	Name           string             `yaml:"name" validate:"required"`
	ShutdownOnExit *bool              `yaml:"shutdown_on_exit"`
	Replicas       int                `yaml:"replicas" validate:"min=0"`
	Runner         *RunnerConfig      `yaml:"runner" validate:"required"`
	Firecracker    *FirecrackerConfig `yaml:"firecracker" validate:"required"`
}

// UnmarshalYAML implements custom unmarshaling to set defaults.
func (p *PoolConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type poolConfigAlias PoolConfig
	defaults := poolConfigAlias{
		ShutdownOnExit: func() *bool { b := true; return &b }(),
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*p = PoolConfig(defaults)
	return nil
}

// NewPool creates a new Pool that gets VMs from backend and its size from demand.
func NewPool(logger *zerolog.Logger, config *PoolConfig, backend VMBackend, demand DemandSource) (*Pool, error) {
	l := logger.With().Str("pool", config.Name).Logger()

	ctx, cancel := context.WithCancel(context.Background())

	p := &Pool{
		config:       config,
		backend:      backend,
		demand:       demand,
		logger:       &l,
		vms:          make(map[string]*poolVM),
		scaleTrigger: make(chan struct{}, 1),
		stopCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}

	p.active.Store(true)
	p.desired.Store(int32(demand.Desired(ctx)))

	metricPoolRunnersCurrent.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(p.GetCurrentSize()))
	metricPoolRunnersDesired.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(p.desired.Load()))
	metricPoolStatus.
		WithLabelValues(p.config.Name).Set(1)

	metricPoolsTotal.Inc()

	return p, nil
}

// Run starts the pool. Starting the pool will start the scaling process.
func (p *Pool) Run() {
	defer close(p.doneCh) // Signal that Run() has exited

	// Trigger initial scale
	p.TriggerScale()

	for {
		select {
		case <-p.scaleTrigger:
		case <-time.After(2 * time.Second):
		case <-p.stopCh:
			return
		case <-p.ctx.Done():
			return
		}

		// Check if we should stop before scaling (non-blocking check)
		select {
		case <-p.ctx.Done():
			return
		case <-p.stopCh:
			return
		default:
		}

		p.reconcile()
	}
}

// reconcile runs one step of the scaling loop. VM creation is asynchronous;
// stops are synchronous.
func (p *Pool) reconcile() {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	if p.ctx.Err() != nil {
		return
	}

	desired := p.demand.Desired(p.ctx)
	p.desired.Store(int32(desired))

	curSize := p.GetCurrentSize()
	pendingCreates := int(p.pendingCreates.Load())
	metricPoolRunnersCurrent.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(curSize))
	metricPoolRunnersDesired.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(desired))
	metricPoolRunnersPending.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(pendingCreates))

	if !p.active.Load() {
		p.logger.Debug().Msgf("Pool %s is paused, skipping scaling", p.config.Name)
		return
	}

	// Effective size accounts for in-flight creates
	delta := desired - (curSize + pendingCreates)
	switch {
	case delta > 0:
		p.logger.Debug().Msgf("Scaling up by %d VMs (target: %d, current: %d, pending creates: %d)",
			delta, desired, curSize, pendingCreates)
		p.scaleUp(delta)
	case delta < 0:
		p.logger.Debug().Msgf("Scaling down by %d VMs (target: %d, current: %d, pending creates: %d)",
			-delta, desired, curSize, pendingCreates)
		p.scaleDown(-delta)
	}
}

func (p *Pool) scaleUp(count int) {
	for i := 0; i < count; i++ {
		p.pendingCreates.Add(1)
		p.creates.Add(1)

		go func() {
			defer p.creates.Done()
			defer p.pendingCreates.Add(-1)

			if p.ctx.Err() != nil {
				return
			}

			start := time.Now()
			vm, err := p.backend.CreateVM(p.ctx)
			if err != nil {
				metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up", "failure").Inc()
				p.logger.Error().Err(err).Msg("Failed to create machine")
				return
			}

			// Add to the pool before pendingCreates drops, so the VM is never uncounted.
			p.track(vm)

			metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up", "success").Inc()
			metricScaleDuration.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up").Observe(time.Since(start).Seconds())
		}()
	}
}

// track adds vm to the pool and removes it again once it has exited.
func (p *Pool) track(vm VM) {
	name := vm.Info().Name

	p.vmsMu.Lock()
	p.vms[name] = &poolVM{vm: vm}
	p.vmsMu.Unlock()

	go func() {
		<-vm.Done()

		p.vmsMu.Lock()
		delete(p.vms, name)
		p.vmsMu.Unlock()

		p.logger.Debug().Msgf("VM %s exited, removed from pool", name)
		p.TriggerScale()
	}()
}

func (p *Pool) scaleDown(count int) {
	victims := p.pickVictims(count)

	for _, v := range victims {
		name := v.vm.Info().Name
		start := time.Now()

		if err := v.vm.Stop(p.ctx); err != nil {
			p.vmsMu.Lock()
			v.stopping = false
			p.vmsMu.Unlock()

			metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down", "failure").Inc()
			p.logger.Warn().Err(err).Msgf("Failed to stop VM %s", name)
			continue
		}

		metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down", "success").Inc()
		metricScaleDuration.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down").Observe(time.Since(start).Seconds())
		p.logger.Info().Msgf("Successfully removed VM %s", name)
	}
}

// pickVictims marks up to count VMs as stopping and returns them.
func (p *Pool) pickVictims(count int) []*poolVM {
	p.vmsMu.Lock()
	defer p.vmsMu.Unlock()

	victims := make([]*poolVM, 0, count)
	for _, v := range p.vms {
		if len(victims) == count {
			break
		}
		if v.stopping {
			continue
		}

		v.stopping = true
		victims = append(victims, v)
	}

	return victims
}

// Stop stops the pool. Stopping the pool will stop all the VMs in the pool.
func (p *Pool) Stop() {
	p.logger.Debug().Msgf("Stopping pool %s", p.config.Name)
	p.cancel()

	// Signal the Run() loop to exit (non-blocking)
	select {
	case p.stopCh <- struct{}{}:
	default:
		// Channel already has a value or Run() already exited
	}

	// Wait for Run() loop to exit cleanly with a timeout
	select {
	case <-p.doneCh:
	case <-time.After(5 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for Run() to exit")
	}

	vms := p.listVMs()
	p.logger.Debug().Msgf("Stopping %d machines in pool %s", len(vms), p.config.Name)

	for _, vm := range vms {
		name := vm.Info().Name
		if err := vm.Kill(); err != nil {
			p.logger.Error().Err(err).Msgf("Failed to stop Firecracker VM %s", name)
		}

		p.logger.Debug().Msgf("Stopped Firecracker VM %s", name)
	}

	cleanupDone := make(chan struct{})
	go func() {
		p.creates.Wait()
		for _, vm := range vms {
			<-vm.Done()
		}
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
	case <-time.After(35 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for cleanup goroutines to finish")
	}

	p.logger.Debug().Msgf("Pool %s stopped", p.config.Name)
}

// Pause pauses the pool. Pausing the pool will prevent the pool from scaling.
func (p *Pool) Pause() {
	if p.active.CompareAndSwap(true, false) {
		p.logger.Debug().Msgf("Pool %s state changed to paused", p.config.Name)
	}
}

// Resume resumes the pool. Resuming the pool will allow the pool to scale.
func (p *Pool) Resume() {
	if p.active.CompareAndSwap(false, true) {
		p.logger.Debug().Msgf("Pool %s state changed to active", p.config.Name)
		p.TriggerScale()
	}
}

// IsActive reports whether the pool is active (not paused).
func (p *Pool) IsActive() bool {
	return p.active.Load()
}

// SetReplicas sets the desired VM count of a pool with fixed demand.
func (p *Pool) SetReplicas(replicas int) error {
	fixed, ok := p.demand.(*fixedDemand)
	if !ok {
		return fmt.Errorf("pool %s is not scaled by replicas", p.config.Name)
	}

	fixed.Set(replicas)
	p.TriggerScale()
	return nil
}

// TriggerScale sends a non-blocking notification to trigger scaling.
func (p *Pool) TriggerScale() {
	select {
	case p.scaleTrigger <- struct{}{}:
	default:
	}
}

// GetReplicas returns the configured replicas for fixed demand, otherwise the
// last desired count.
func (p *Pool) GetReplicas() int {
	if fixed, ok := p.demand.(*fixedDemand); ok {
		return fixed.Desired(p.ctx)
	}

	return p.GetDesired()
}

// GetDesired returns the desired VM count computed by the last reconcile.
func (p *Pool) GetDesired() int {
	return int(p.desired.Load())
}

// GetCurrentSize returns the number of VMs in the pool, not counting VMs being stopped.
func (p *Pool) GetCurrentSize() int {
	p.vmsMu.Lock()
	defer p.vmsMu.Unlock()

	n := 0
	for _, v := range p.vms {
		if !v.stopping {
			n++
		}
	}

	return n
}

func (p *Pool) listVMs() []VM {
	p.vmsMu.Lock()
	defer p.vmsMu.Unlock()

	vms := make([]VM, 0, len(p.vms))
	for _, v := range p.vms {
		vms = append(vms, v.vm)
	}

	return vms
}

// ListMachines returns all VMs of the pool.
func (p *Pool) ListMachines(_ context.Context) ([]VM, error) {
	return p.listVMs(), nil
}

// GetMachine returns the VM with the given name.
func (p *Pool) GetMachine(name string) (VM, error) {
	p.vmsMu.Lock()
	defer p.vmsMu.Unlock()

	v, ok := p.vms[name]
	if !ok {
		return nil, fmt.Errorf("machine not found: %s", name)
	}

	return v.vm, nil
}
