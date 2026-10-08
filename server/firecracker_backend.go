package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/hostinger/fireactions/helper/deepcopy"
	"github.com/hostinger/fireactions/helper/github"
	"github.com/hostinger/fireactions/helper/stringid"
	"github.com/opencontainers/image-spec/identity"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"

	githubv63 "github.com/google/go-github/v63/github"
)

const (
	defaultSnapshotter = "devmapper"
)

// firecrackerBackend is the real VMBackend: containerd snapshots, Firecracker
// VMMs and GitHub JIT runner registration.
type firecrackerBackend struct {
	config         *PoolConfig
	containerd     *containerd.Client
	github         *github.Client
	imageManager   *imageManager
	installationID atomic.Int64
	nextCID        *atomic.Uint32
	logger         *zerolog.Logger
}

var _ VMBackend = (*firecrackerBackend)(nil)

func newFirecrackerBackend(logger *zerolog.Logger, config *PoolConfig, github *github.Client, imageManager *imageManager, containerdClient *containerd.Client, nextCID *atomic.Uint32) (*firecrackerBackend, error) {
	l := logger.With().Str("pool", config.Name).Logger()

	b := &firecrackerBackend{
		config:       config,
		containerd:   containerdClient,
		github:       github,
		imageManager: imageManager,
		nextCID:      nextCID,
		logger:       &l,
	}

	if _, err := os.Stat(b.dir()); os.IsNotExist(err) {
		if err := os.MkdirAll(b.dir(), 0755); err != nil {
			return nil, fmt.Errorf("creating pool directory: %w", err)
		}

		b.logger.Debug().Msgf("Pool directory created at %s", b.dir())
	}

	return b, nil
}

// dir returns the directory where the pool sockets and logs are stored.
func (b *firecrackerBackend) dir() string {
	return fmt.Sprintf("/var/lib/fireactions/pools/%s", b.config.Name)
}

// CreateVM implements VMBackend.
func (b *firecrackerBackend) CreateVM(ctx context.Context) (VM, error) {
	image, err := b.imageManager.ensureImage(
		ctx,
		b.config.Runner.Image,
		b.config.Runner.ImagePullPolicy,
	)
	if err != nil {
		return nil, fmt.Errorf("ensuring image: %w", err)
	}

	runnerName := fmt.Sprintf("%s-%s", b.config.Runner.Name, stringid.New())

	leaseCtx, leaseCtxCancel, err := b.containerd.WithLease(ctx,
		leases.WithID(fmt.Sprintf("fireactions/pools/%s/%s", b.config.Name, runnerName)))
	if err != nil {
		return nil, fmt.Errorf("containerd: creating lease: %w", err)
	}

	// Track if we successfully created the machine to determine cleanup responsibility
	var machineCreated bool
	defer func() {
		if !machineCreated {
			// Clean up lease if machine creation failed
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = leaseCtxCancel(cleanupCtx)
		}
	}()

	snapshotMounts, err := b.createSnapshot(leaseCtx, image, runnerName)
	if err != nil {
		return nil, fmt.Errorf("containerd: creating snapshot: %w", err)
	}

	machineLogFile, err := os.Create(filepath.Join(b.dir(), fmt.Sprintf("%s.log", runnerName)))
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}
	defer machineLogFile.Close()

	machineCmd := firecracker.VMCommandBuilder{}.
		WithSocketPath(filepath.Join(b.dir(), fmt.Sprintf("%s.sock", runnerName))).
		WithStderr(machineLogFile).
		WithStdout(machineLogFile).
		WithBin(b.config.Firecracker.BinaryPath).
		Build(ctx)

	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(io.Discard)

	vsockPath := filepath.Join(b.dir(), fmt.Sprintf("%s.vsock", runnerName))
	vsockCID := b.nextCID.Add(1)

	fcMachine, err := firecracker.NewMachine(ctx, firecracker.Config{
		VMID:            runnerName,
		SocketPath:      filepath.Join(b.dir(), fmt.Sprintf("%s.sock", runnerName)),
		KernelImagePath: b.config.Firecracker.KernelImagePath,
		KernelArgs:      b.config.Firecracker.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &b.config.Firecracker.MachineConfig.VcpuCount,
			MemSizeMib: &b.config.Firecracker.MachineConfig.MemSizeMib,
		},
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   &snapshotMounts[0].Source,
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		NetworkInterfaces: []firecracker.NetworkInterface{{
			AllowMMDS:        true,
			CNIConfiguration: &firecracker.CNIConfiguration{NetworkName: "fireactions", IfName: "eth0", ConfDir: "/etc/cni/net.d", BinPath: []string{"/opt/cni/bin"}},
		}},
		VsockDevices:   []firecracker.VsockDevice{{Path: vsockPath, CID: vsockCID}},
		MmdsAddress:    net.IPv4(169, 254, 169, 254),
		MmdsVersion:    firecracker.MMDSv2,
		ForwardSignals: []os.Signal{},
		LogPath:        filepath.Join(b.dir(), fmt.Sprintf("%s.firecracker.log", runnerName)),
		LogLevel:       "Debug",
	}, firecracker.WithProcessRunner(machineCmd), firecracker.WithLogger(logrus.NewEntry(logger)))
	if err != nil {
		return nil, fmt.Errorf("firecracker: creating machine: %w", err)
	}

	installationID, err := b.getInstallationID(ctx)
	if err != nil {
		return nil, err
	}

	client := b.github.Installation(installationID)
	jitReq := &githubv63.GenerateJITConfigRequest{
		Name:          runnerName,
		RunnerGroupID: b.config.Runner.GroupID,
		Labels:        b.config.Runner.Labels,
	}
	var jitConfig *githubv63.JITRunnerConfig
	if repo := b.config.Runner.Repository; repo != "" {
		jitConfig, _, err = client.Actions.GenerateRepoJITConfig(ctx, b.config.Runner.Organization, repo, jitReq)
	} else {
		jitConfig, _, err = client.Actions.GenerateOrgJITConfig(ctx, b.config.Runner.Organization, jitReq)
	}
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}

	metadata := map[string]interface{}{"latest": map[string]interface{}{"meta-data": deepcopy.Map(b.config.Firecracker.Metadata)}}
	metadata["latest"].(map[string]interface{})["meta-data"].(map[string]interface{})["fireactions"] = map[string]interface{}{
		"runner_id":         runnerName,
		"runner_jit_config": jitConfig.GetEncodedJITConfig(),
		"hostname":          runnerName,
		"shutdown_on_exit":  *b.config.ShutdownOnExit,
	}

	fcMachine.Handlers.FcInit = fcMachine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(metadata))

	vmmCtx, vmmCancel := context.WithCancel(ctx)
	if err := fcMachine.Start(vmmCtx); err != nil {
		vmmCancel()
		return nil, fmt.Errorf("firecracker: starting machine: %w", err)
	}

	// Mark machine as successfully created
	machineCreated = true

	b.logger.Info().Msgf("Successfully created Firecracker VM %s", runnerName)

	machine := &Machine{
		Machine:     fcMachine,
		Name:        runnerName,
		RunnerID:    jitConfig.GetRunner().GetID(),
		Pool:        b.config.Name,
		CreatedAt:   time.Now().UTC(),
		vsockCID:    vsockCID,
		vsockPath:   vsockPath,
		leaseCancel: leaseCtxCancel,
		vmmCtx:      vmmCtx,
		vmmCancel:   vmmCancel,
		done:        make(chan struct{}),
	}
	var deregistered atomic.Bool
	machine.deregister = func(ctx context.Context) error {
		if deregistered.Load() {
			return nil
		}
		if err := b.deleteGitHubRunner(ctx, runnerName, machine.RunnerID); err != nil {
			return err
		}
		deregistered.Store(true)
		return nil
	}

	go b.cleanupOnExit(ctx, machine)

	return machine, nil
}

// cleanupOnExit waits for the VM to exit, releases its resources and closes
// machine.done.
func (b *firecrackerBackend) cleanupOnExit(ctx context.Context, machine *Machine) {
	defer close(machine.done)

	runnerName := machine.Name

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- machine.Wait(context.Background())
	}()

	select {
	case <-waitDone:
		// Machine exited normally
	case <-ctx.Done():
		// Pool is stopping, wait up to 30s for machine to fully exit
		select {
		case <-waitDone:
		case <-time.After(30 * time.Second):
			b.logger.Warn().Msgf("Timeout waiting for machine %s to exit during pool shutdown", runnerName)
		}
	}

	machine.vmmCancel()

	deregisterCtx, deregisterCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer deregisterCancel()
	if err := machine.deregister(deregisterCtx); err != nil {
		b.logger.Error().Err(err).Msgf("Failed to delete GitHub runner %s (ID: %d)", runnerName, machine.RunnerID)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := machine.leaseCancel(cleanupCtx)
	if err != nil && !errdefs.IsNotFound(err) {
		b.logger.Error().Err(err).Msgf("Failed to remove Containerd lease for Firecracker VM %s", runnerName)
	}

	b.logger.Info().Msgf("Successfully cleaned up exited Firecracker VM %s", runnerName)
}

func (b *firecrackerBackend) getInstallationID(ctx context.Context) (int64, error) {
	if id := b.installationID.Load(); id != 0 {
		return id, nil
	}

	var installation *githubv63.Installation
	var err error
	if repo := b.config.Runner.Repository; repo != "" {
		installation, _, err = b.github.Apps.FindRepositoryInstallation(ctx, b.config.Runner.Organization, repo)
	} else {
		installation, _, err = b.github.Apps.FindOrganizationInstallation(ctx, b.config.Runner.Organization)
	}
	if err != nil {
		return 0, fmt.Errorf("github: %w", err)
	}

	b.installationID.Store(installation.GetID())
	return installation.GetID(), nil
}

// createSnapshot creates a snapshot of the specified image.
func (b *firecrackerBackend) createSnapshot(ctx context.Context, image containerd.Image, snapshotID string) ([]mount.Mount, error) {
	snapshotService := b.containerd.SnapshotService(defaultSnapshotter)
	snapshotExists := true
	_, err := snapshotService.Stat(ctx, snapshotID)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}

		snapshotExists = false
	}

	if !snapshotExists {
		imageContent, err := image.RootFS(ctx)
		if err != nil {
			return nil, fmt.Errorf("image: rootfs: %w", err)
		}

		_, err = snapshotService.Prepare(ctx, snapshotID, identity.ChainID(imageContent).String())
		if err != nil {
			return nil, fmt.Errorf("prepare: %w", err)
		}
	}

	mounts, err := snapshotService.Mounts(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("mounts: %w", err)
	}

	return mounts, nil
}

// deleteGitHubRunner removes a runner from GitHub Actions. GitHub refuses to
// remove a runner that is running a job.
func (b *firecrackerBackend) deleteGitHubRunner(ctx context.Context, runnerName string, runnerID int64) error {
	if runnerID == 0 {
		b.logger.Debug().Msgf("No GitHub runner ID found for %s, skipping deletion", runnerName)
		return nil
	}

	if b.installationID.Load() == 0 {
		return fmt.Errorf("no installation ID available, cannot delete runner %s", runnerName)
	}

	client := b.github.Installation(b.installationID.Load())

	var err error
	if repo := b.config.Runner.Repository; repo != "" {
		_, err = client.Actions.RemoveRunner(ctx, b.config.Runner.Organization, repo, runnerID)
	} else {
		_, err = client.Actions.RemoveOrganizationRunner(ctx, b.config.Runner.Organization, runnerID)
	}
	if err != nil {
		return err
	}

	b.logger.Debug().Msgf("Successfully deleted GitHub runner %s (ID: %d)", runnerName, runnerID)
	return nil
}

func init() {
	_ = log.SetLevel("panic")
}
