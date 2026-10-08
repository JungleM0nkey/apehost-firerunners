package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	githubv63 "github.com/google/go-github/v63/github"
	"github.com/hostinger/fireactions/helper/github"
)

// runnerRegistrar registers the runner of a new VM with GitHub and removes it
// again. Removal fails while the runner is running a job.
type runnerRegistrar interface {
	// Register returns the encoded JIT config and the runner ID.
	Register(ctx context.Context, name string) (encodedJITConfig string, runnerID int64, err error)
	Deregister(ctx context.Context, runnerID int64) error
}

// errPublicRepository is returned when a pool targets a public repository:
// anyone could then run code on our hosts through a pull request.
var errPublicRepository = errors.New("refusing to register runners for a public repository")

// installationFinder finds the GitHub App installation for the pool's owner or
// repository, caches it, and checks once that a repository is private.
type installationFinder struct {
	github *github.Client
	runner *RunnerConfig

	mu      sync.Mutex
	id      atomic.Int64
	checked bool
}

func (f *installationFinder) installationID(ctx context.Context) (int64, error) {
	if id := f.id.Load(); id != 0 {
		return id, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if id := f.id.Load(); id != 0 {
		return id, nil
	}

	var installation *githubv63.Installation
	var err error
	if repo := f.runner.Repository; repo != "" {
		installation, _, err = f.github.Apps.FindRepositoryInstallation(ctx, f.runner.Organization, repo)
	} else {
		installation, _, err = f.github.Apps.FindOrganizationInstallation(ctx, f.runner.Organization)
	}
	if err != nil {
		return 0, fmt.Errorf("github: %w", err)
	}

	if repo := f.runner.Repository; repo != "" && !f.checked {
		r, _, err := f.github.Installation(installation.GetID()).Repositories.Get(ctx, f.runner.Organization, repo)
		if err != nil {
			return 0, fmt.Errorf("github: checking repository visibility: %w", err)
		}
		if !r.GetPrivate() {
			return 0, fmt.Errorf("%w: %s/%s", errPublicRepository, f.runner.Organization, repo)
		}
		f.checked = true
	}

	f.id.Store(installation.GetID())
	return installation.GetID(), nil
}

// restRegistrar registers plain JIT runners through the REST API (fixed pools).
type restRegistrar struct {
	installations *installationFinder
	runner        *RunnerConfig
}

var _ runnerRegistrar = (*restRegistrar)(nil)

func (r *restRegistrar) Register(ctx context.Context, name string) (string, int64, error) {
	installationID, err := r.installations.installationID(ctx)
	if err != nil {
		return "", 0, err
	}

	client := r.installations.github.Installation(installationID)
	jitReq := &githubv63.GenerateJITConfigRequest{
		Name:          name,
		RunnerGroupID: r.runner.GroupID,
		Labels:        r.runner.Labels,
	}

	var jitConfig *githubv63.JITRunnerConfig
	if repo := r.runner.Repository; repo != "" {
		jitConfig, _, err = client.Actions.GenerateRepoJITConfig(ctx, r.runner.Organization, repo, jitReq)
	} else {
		jitConfig, _, err = client.Actions.GenerateOrgJITConfig(ctx, r.runner.Organization, jitReq)
	}
	if err != nil {
		return "", 0, fmt.Errorf("github: %w", err)
	}

	return jitConfig.GetEncodedJITConfig(), jitConfig.GetRunner().GetID(), nil
}

func (r *restRegistrar) Deregister(ctx context.Context, runnerID int64) error {
	installationID, err := r.installations.installationID(ctx)
	if err != nil {
		return err
	}

	client := r.installations.github.Installation(installationID)
	if repo := r.runner.Repository; repo != "" {
		_, err = client.Actions.RemoveRunner(ctx, r.runner.Organization, repo, runnerID)
	} else {
		_, err = client.Actions.RemoveOrganizationRunner(ctx, r.runner.Organization, runnerID)
	}

	return err
}
