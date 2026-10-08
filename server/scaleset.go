package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/hostinger/fireactions"
	"github.com/hostinger/fireactions/helper/github"
	"github.com/rs/zerolog"
)

// ScaleSetConfig turns a pool into a GitHub runner scale set: GitHub tells the
// host how many jobs are assigned to it, and the pool sizes itself to match.
// Each host registers its own scale set and advertises its own capacity (the
// pool's max), so GitHub does the cross-host matching.
type ScaleSetConfig struct {
	// Name of the scale set; unique per runner group, so per host. Defaults to
	// "<pool name>-<hostname>".
	Name string `yaml:"name"`
	// Labels workflows target with runs-on. Defaults to runner.labels.
	Labels []string `yaml:"labels"`
	// RunnerGroup defaults to "default" (the only group on personal accounts).
	RunnerGroup string `yaml:"runner_group"`
}

// errScaleSetNotReady is returned when a VM needs a JIT config before the
// scale set is registered.
var errScaleSetNotReady = errors.New("scale set not registered yet")

// scaleSetSession is an open connection to a registered scale set.
type scaleSetSession struct {
	scaleSetID int
	client     listener.Client
	close      func()
}

// scaleSetConn is the GitHub side of a scale set. The real implementation wraps
// github.com/actions/scaleset; tests use a fake.
type scaleSetConn interface {
	// Open registers (or updates) the scale set and opens a message session.
	Open(ctx context.Context) (*scaleSetSession, error)
	GenerateJIT(ctx context.Context, scaleSetID int, name string) (encodedJITConfig string, runnerID int64, err error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// scaleSetDemand is a DemandSource driven by a scale set's assigned-job
// statistics. It is also the pool's runnerRegistrar, since scale-set runners
// get their JIT configs from the scale set.
type scaleSetDemand struct {
	conn       scaleSetConn
	maxRunners int
	logger     *zerolog.Logger
	onChange   func()
	minBackoff time.Duration
	maxBackoff time.Duration

	assigned   atomic.Int32
	scaleSetID atomic.Int64

	busyMu sync.Mutex
	busy   map[string]bool
}

var (
	_ DemandSource    = (*scaleSetDemand)(nil)
	_ runnerRegistrar = (*scaleSetDemand)(nil)
	_ listener.Scaler = (*scaleSetDemand)(nil)
)

func newScaleSetDemand(logger *zerolog.Logger, conn scaleSetConn, maxRunners int) *scaleSetDemand {
	return &scaleSetDemand{
		conn:       conn,
		maxRunners: maxRunners,
		logger:     logger,
		onChange:   func() {},
		minBackoff: time.Second,
		maxBackoff: time.Minute,
		busy:       make(map[string]bool),
	}
}

// Desired implements DemandSource: the number of jobs assigned to this host's
// scale set. The pool clamps it to its min and max.
func (d *scaleSetDemand) Desired(context.Context) int { return int(d.assigned.Load()) }

// IsBusy reports whether GitHub said a job started on the runner and it hasn't
// completed. The pool never scales such a VM down, even if its agent lags.
func (d *scaleSetDemand) IsBusy(name string) bool {
	d.busyMu.Lock()
	defer d.busyMu.Unlock()
	return d.busy[name]
}

// Run keeps a listener session open until ctx is cancelled, reconnecting with
// exponential backoff after network or GitHub errors.
func (d *scaleSetDemand) Run(ctx context.Context) {
	backoff := d.minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := d.listen(ctx)
		if ctx.Err() != nil {
			return
		}

		if time.Since(start) > d.maxBackoff {
			backoff = d.minBackoff // the session was healthy for a while
		}
		d.logger.Warn().Err(err).Msgf("Scale set listener stopped, reconnecting in %s", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, d.maxBackoff)
	}
}

func (d *scaleSetDemand) listen(ctx context.Context) error {
	session, err := d.conn.Open(ctx)
	if err != nil {
		return fmt.Errorf("opening scale set session: %w", err)
	}
	defer session.close()

	d.scaleSetID.Store(int64(session.scaleSetID))
	d.logger.Info().Msgf("Scale set %d session open, max capacity %d", session.scaleSetID, d.maxRunners)

	l, err := listener.New(session.client, listener.Config{
		ScaleSetID: session.scaleSetID,
		MaxRunners: d.maxRunners,
		Logger:     slog.New(newZerologHandler(d.logger)),
	})
	if err != nil {
		return err
	}

	return l.Run(ctx, d)
}

// HandleDesiredRunnerCount implements listener.Scaler.
func (d *scaleSetDemand) HandleDesiredRunnerCount(_ context.Context, count int) (int, error) {
	if old := d.assigned.Swap(int32(count)); int(old) != count {
		d.logger.Info().Msgf("Scale set assigned jobs: %d -> %d", old, count)
		d.onChange()
	}
	return count, nil
}

// HandleJobStarted implements listener.Scaler.
func (d *scaleSetDemand) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	d.busyMu.Lock()
	d.busy[job.RunnerName] = true
	d.busyMu.Unlock()

	d.logger.Info().Msgf("Job %q (%s) started on VM %s", job.JobDisplayName, job.JobID, job.RunnerName)
	return nil
}

// HandleJobCompleted implements listener.Scaler.
func (d *scaleSetDemand) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	d.busyMu.Lock()
	delete(d.busy, job.RunnerName)
	d.busyMu.Unlock()

	d.logger.Info().Msgf("Job %q (%s) completed on VM %s: %s", job.JobDisplayName, job.JobID, job.RunnerName, job.Result)
	return nil
}

// Register implements runnerRegistrar.
func (d *scaleSetDemand) Register(ctx context.Context, name string) (string, int64, error) {
	id := int(d.scaleSetID.Load())
	if id == 0 {
		return "", 0, errScaleSetNotReady
	}
	return d.conn.GenerateJIT(ctx, id, name)
}

// Deregister implements runnerRegistrar. GitHub refuses while a job runs.
func (d *scaleSetDemand) Deregister(ctx context.Context, runnerID int64) error {
	return d.conn.RemoveRunner(ctx, runnerID)
}

// githubScaleSetConn is the real scaleSetConn.
type githubScaleSetConn struct {
	pool          string
	settings      ScaleSetConfig
	configURL     string
	owner         string // session owner, the hostname
	appID         int64
	appKey        string
	installations *installationFinder

	mu     sync.Mutex
	client *scaleset.Client
}

var _ scaleSetConn = (*githubScaleSetConn)(nil)

func newGitHubScaleSetConn(pool *PoolConfig, gh *GitHubConfig, ghClient *github.Client, hostname string) *githubScaleSetConn {
	settings := *pool.ScaleSet
	if settings.Name == "" {
		settings.Name = pool.Name + "-" + hostname
	}
	if len(settings.Labels) == 0 {
		settings.Labels = pool.Runner.Labels
	}
	if settings.RunnerGroup == "" {
		settings.RunnerGroup = scaleset.DefaultRunnerGroup
	}

	configURL := "https://github.com/" + pool.Runner.Organization
	if pool.Runner.Repository != "" {
		configURL += "/" + pool.Runner.Repository
	}

	return &githubScaleSetConn{
		pool:          pool.Name,
		settings:      settings,
		configURL:     configURL,
		owner:         hostname,
		appID:         gh.AppID,
		appKey:        gh.AppPrivateKey,
		installations: &installationFinder{github: ghClient, runner: pool.Runner},
	}
}

func (c *githubScaleSetConn) scalesetClient(ctx context.Context) (*scaleset.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return c.client, nil
	}

	installationID, err := c.installations.installationID(ctx)
	if err != nil {
		return nil, err
	}

	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.configURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       strconv.FormatInt(c.appID, 10), // the App ID works as client ID
			InstallationID: installationID,
			PrivateKey:     c.appKey,
		},
		SystemInfo: scaleset.SystemInfo{System: "fireactions", Version: fireactions.Version, CommitSHA: fireactions.Commit, Subsystem: "pool/" + c.pool},
	})
	if err != nil {
		return nil, err
	}

	c.client = client
	return client, nil
}

// Open implements scaleSetConn.
func (c *githubScaleSetConn) Open(ctx context.Context) (*scaleSetSession, error) {
	client, err := c.scalesetClient(ctx)
	if err != nil {
		return nil, err
	}

	groupID := 1 // the default group always has ID 1
	if c.settings.RunnerGroup != scaleset.DefaultRunnerGroup {
		group, err := client.GetRunnerGroupByName(ctx, c.settings.RunnerGroup)
		if err != nil {
			return nil, fmt.Errorf("runner group %q: %w", c.settings.RunnerGroup, err)
		}
		groupID = group.ID
	}

	want := &scaleset.RunnerScaleSet{
		Name:          c.settings.Name,
		RunnerGroupID: groupID,
		Labels:        make([]scaleset.Label, 0, len(c.settings.Labels)),
		// The runner image is rebuilt for every runner release (#9), so
		// runners never need to update themselves at boot.
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	for _, label := range c.settings.Labels {
		want.Labels = append(want.Labels, scaleset.Label{Name: label})
	}

	existing, err := client.GetRunnerScaleSet(ctx, groupID, c.settings.Name)
	if err != nil {
		return nil, fmt.Errorf("getting scale set %q: %w", c.settings.Name, err)
	}

	var ss *scaleset.RunnerScaleSet
	if existing == nil {
		ss, err = client.CreateRunnerScaleSet(ctx, want)
	} else {
		ss, err = client.UpdateRunnerScaleSet(ctx, existing.ID, want)
	}
	if err != nil {
		return nil, fmt.Errorf("registering scale set %q (labels %s): %w", c.settings.Name, strings.Join(c.settings.Labels, ","), err)
	}

	info := client.SystemInfo()
	info.ScaleSetID = ss.ID
	client.SetSystemInfo(info)

	session, err := client.MessageSessionClient(ctx, ss.ID, c.owner)
	if err != nil {
		return nil, fmt.Errorf("scale set %d: %w", ss.ID, err)
	}

	return &scaleSetSession{
		scaleSetID: ss.ID,
		client:     session,
		close: func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = session.Close(closeCtx)
		},
	}, nil
}

// GenerateJIT implements scaleSetConn.
func (c *githubScaleSetConn) GenerateJIT(ctx context.Context, scaleSetID int, name string) (string, int64, error) {
	client, err := c.scalesetClient(ctx)
	if err != nil {
		return "", 0, err
	}

	jit, err := client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, scaleSetID)
	if err != nil {
		return "", 0, fmt.Errorf("generating JIT config: %w", err)
	}

	var runnerID int64
	if jit.Runner != nil {
		runnerID = int64(jit.Runner.ID)
	}
	return jit.EncodedJITConfig, runnerID, nil
}

// RemoveRunner implements scaleSetConn.
func (c *githubScaleSetConn) RemoveRunner(ctx context.Context, runnerID int64) error {
	client, err := c.scalesetClient(ctx)
	if err != nil {
		return err
	}
	return client.RemoveRunner(ctx, runnerID)
}

// zerologHandler sends the scaleset library's slog output to zerolog. Its
// per-poll Info lines are demoted to Debug.
type zerologHandler struct {
	logger *zerolog.Logger
	attrs  []slog.Attr
}

func newZerologHandler(logger *zerolog.Logger) *zerologHandler {
	return &zerologHandler{logger: logger}
}

func (h *zerologHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.zlevel(level) >= h.logger.GetLevel()
}

func (h *zerologHandler) zlevel(level slog.Level) zerolog.Level {
	switch {
	case level >= slog.LevelError:
		return zerolog.ErrorLevel
	case level >= slog.LevelWarn:
		return zerolog.WarnLevel
	default:
		return zerolog.DebugLevel
	}
}

func (h *zerologHandler) Handle(_ context.Context, r slog.Record) error {
	e := h.logger.WithLevel(h.zlevel(r.Level))
	for _, a := range h.attrs {
		e = e.Interface(a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		e = e.Interface(a.Key, a.Value.Any())
		return true
	})
	e.Msg(r.Message)
	return nil
}

func (h *zerologHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &zerologHandler{logger: h.logger, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *zerologHandler) WithGroup(string) slog.Handler { return h }
