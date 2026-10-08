package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSession is a listener.Client that replays queued messages, then fails
// (if failAfter is set) or long-polls until the context ends.
type fakeSession struct {
	mu          sync.Mutex
	initial     int
	messages    []*scaleset.RunnerScaleSetMessage
	failAfter   bool
	maxCapacity int
	closed      bool
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{
		SessionID:  uuid.New(),
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: s.initial},
	}
}

func (s *fakeSession) GetMessage(ctx context.Context, _, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	s.maxCapacity = maxCapacity
	if len(s.messages) > 0 {
		msg := s.messages[0]
		s.messages = s.messages[1:]
		s.mu.Unlock()
		return msg, nil
	}
	fail := s.failAfter
	s.mu.Unlock()

	if fail {
		return nil, errors.New("connection reset by peer")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *fakeSession) DeleteMessage(context.Context, int) error { return nil }

func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) { return ids, nil }

// fakeScaleSetConn hands out queued sessions; a nil entry fails Open.
type fakeScaleSetConn struct {
	mu       sync.Mutex
	sessions []*fakeSession
	opens    int
	removed  []int64
	jitFor   []int
}

func (c *fakeScaleSetConn) Open(context.Context) (*scaleSetSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.opens++
	if len(c.sessions) == 0 {
		return nil, errors.New("no more sessions")
	}
	s := c.sessions[0]
	c.sessions = c.sessions[1:]
	if s == nil {
		return nil, errors.New("github unreachable")
	}

	return &scaleSetSession{scaleSetID: 42, client: s, close: func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}}, nil
}

func (c *fakeScaleSetConn) GenerateJIT(_ context.Context, scaleSetID int, _ string) (string, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jitFor = append(c.jitFor, scaleSetID)
	return "jit", 7, nil
}

func (c *fakeScaleSetConn) RemoveRunner(_ context.Context, runnerID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, runnerID)
	return nil
}

func (c *fakeScaleSetConn) openCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func statsMessage(id, assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{
		MessageID:  id,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned},
	}
}

func newTestDemand(conn scaleSetConn, maxRunners int) *scaleSetDemand {
	logger := zerolog.Nop()
	d := newScaleSetDemand(&logger, conn, maxRunners)
	d.minBackoff = time.Millisecond
	d.maxBackoff = 5 * time.Millisecond
	return d
}

func intPtr(n int) *int { return &n }

func newScaleSetTestPool(t *testing.T, b *fakeBackend, d *scaleSetDemand, minVMs, maxVMs int) *Pool {
	t.Helper()

	logger := zerolog.Nop()
	p, err := NewPool(&logger, &PoolConfig{
		Name: "test", Runner: &RunnerConfig{Organization: "org"},
		Min: intPtr(minVMs), Max: intPtr(maxVMs), ScaleSet: &ScaleSetConfig{},
	}, b, d)
	require.NoError(t, err)
	t.Cleanup(p.cancel)
	return p
}

func TestScaleSet_AssignedJobsDriveDesiredWithinMinMax(t *testing.T) {
	b := &fakeBackend{}
	d := newTestDemand(&fakeScaleSetConn{}, 3)
	p := newScaleSetTestPool(t, b, d, 1, 3)

	// No jobs: min warm VMs.
	settle(t, p, b)
	assert.Len(t, b.running(), 1)

	// Five jobs assigned, capacity 3.
	_, _ = d.HandleDesiredRunnerCount(t.Context(), 5)
	settle(t, p, b)
	assert.Len(t, b.running(), 3)

	// Jobs done: back down to min, stopping idle VMs only.
	_, _ = d.HandleDesiredRunnerCount(t.Context(), 0)
	settle(t, p, b)
	assert.Len(t, b.running(), 1)
}

func TestScaleSet_JobStartedPreventsScaleDown(t *testing.T) {
	b := &fakeBackend{}
	d := newTestDemand(&fakeScaleSetConn{}, 2)
	p := newScaleSetTestPool(t, b, d, 0, 2)

	_, _ = d.HandleDesiredRunnerCount(t.Context(), 2)
	settle(t, p, b)
	require.Len(t, b.running(), 2)

	// GitHub says vm-0 started a job; its agent hasn't noticed yet and still
	// reports Idle.
	require.NoError(t, d.HandleJobStarted(t.Context(), &scaleset.JobStarted{RunnerName: "vm-0"}))

	_, _ = d.HandleDesiredRunnerCount(t.Context(), 0)
	settle(t, p, b)
	assert.Equal(t, []string{"vm-0"}, b.running(), "the VM with a job must survive")

	// Job completes; the VM is idle again and may go.
	require.NoError(t, d.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{RunnerName: "vm-0", Result: "succeeded"}))
	settle(t, p, b)
	assert.Empty(t, b.running())
}

func TestScaleSet_ListenerRecoversWithoutRestart(t *testing.T) {
	healthy := &fakeSession{messages: []*scaleset.RunnerScaleSetMessage{statsMessage(2, 4)}}
	conn := &fakeScaleSetConn{sessions: []*fakeSession{
		nil,                           // GitHub unreachable on the first attempt
		{initial: 1, failAfter: true}, // session drops after the initial stats
		healthy,
	}}
	d := newTestDemand(conn, 6)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return d.Desired(ctx) == 4 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 3, conn.openCount())

	healthy.mu.Lock()
	assert.Equal(t, 6, healthy.maxCapacity, "the pool's max is advertised to GitHub")
	healthy.mu.Unlock()

	cancel()
	<-done
	healthy.mu.Lock()
	assert.True(t, healthy.closed, "the session is closed on shutdown")
	healthy.mu.Unlock()
}

func TestScaleSet_RegisterUsesScaleSetOnceOpen(t *testing.T) {
	conn := &fakeScaleSetConn{sessions: []*fakeSession{{}}}
	d := newTestDemand(conn, 1)

	_, _, err := d.Register(t.Context(), "vm-0")
	require.ErrorIs(t, err, errScaleSetNotReady)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.Run(ctx)
	require.Eventually(t, func() bool { _, _, err := d.Register(ctx, "vm-0"); return err == nil }, time.Second, time.Millisecond)

	conn.mu.Lock()
	defer conn.mu.Unlock()
	assert.Equal(t, 42, conn.jitFor[len(conn.jitFor)-1])
}

func TestConfig_ScaleSetValidation(t *testing.T) {
	valid := func() *Config {
		c := DefaultConfig()
		c.GitHub = &GitHubConfig{AppID: 1, AppPrivateKey: "k"}
		c.Pools = []*PoolConfig{{
			Name: "p", Max: intPtr(4), ScaleSet: &ScaleSetConfig{},
			Runner:      &RunnerConfig{Name: "r", ImagePullPolicy: "Never", Image: "i", Organization: "o", GroupID: 1, Labels: []string{"l"}},
			Firecracker: &FirecrackerConfig{},
		}}
		return c
	}

	require.NoError(t, valid().Validate())

	c := valid()
	c.Pools[0].Max = nil
	assert.ErrorContains(t, c.Validate(), "scale_set requires max")

	c = valid()
	c.Pools[0].Replicas = 2
	assert.ErrorContains(t, c.Validate(), "mutually exclusive")

	c = valid()
	c.Pools[0].Min = intPtr(5)
	assert.ErrorContains(t, c.Validate(), "min (5) is greater than max (4)")
}

func TestGitHubScaleSetConn_Defaults(t *testing.T) {
	pool := &PoolConfig{
		Name:     "fireactions-4vcpu-8gb",
		ScaleSet: &ScaleSetConfig{},
		Runner:   &RunnerConfig{Organization: "JungleM0nkey", Repository: "apehost-connect-dashboard", Labels: []string{"self-hosted", "fireactions-4vcpu-8gb"}},
	}

	c := newGitHubScaleSetConn(pool, &GitHubConfig{AppID: 5235650}, nil, "podbox")

	assert.Equal(t, "fireactions-4vcpu-8gb-podbox", c.settings.Name, "one scale set per host")
	assert.Equal(t, []string{"self-hosted", "fireactions-4vcpu-8gb"}, c.settings.Labels)
	assert.Equal(t, "default", c.settings.RunnerGroup)
	assert.Equal(t, "https://github.com/JungleM0nkey/apehost-connect-dashboard", c.configURL)
	assert.Equal(t, "podbox", c.owner)
}
