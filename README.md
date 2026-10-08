# apehost-firerunners

ApeHost's fork of [Fireactions](https://github.com/hostinger/fireactions): ephemeral GitHub Actions runners in Firecracker microVMs, one fresh VM per job. It runs on **podbox** and serves our CI and deploys without GitHub-hosted minutes.

Based on upstream **v2.0.8**. Licensed Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).

## Changes from upstream

- **Repo-level runners** (`runner.repository`): upstream registers org-level runners only, so personal accounts can't use it. When `repository` is set, `organization` is treated as the owner and the server uses `FindRepositoryInstallation` / `GenerateRepoJITConfig` / `RemoveRunner`.
- **Safe scale-down and drain** (#12): scale-down only stops VMs whose runner is idle, after GitHub agrees to remove the runner, so it never kills a job. `pools pause` drains a pool.
- **Per-pool env** (`env:`, #8): variables added to the runner process in every VM of the pool, so every job step sees them. Never logged.
- **Scale sets** (`scale_set:`, `min`, `max`, #14): a pool can size itself from a GitHub runner scale set (one per host, outbound long-poll only) instead of fixed `replicas`.
- **Turborepo cache proxy** ([`cache-proxy/`](cache-proxy/), #13/#15): a workerd read-through cache on the VM bridge, with read-only tokens for PR pools.
- **Fleet config** (`fireactions fleet render`, #16): one [`deploy/fleet.yaml`](deploy/fleet.yaml) rendered into each host's configs. `github.app_private_key_file` keeps the App key out of config files.
- Refuses to register runners for public repositories.
- Removed upstream's release workflows (release-please, goreleaser to `ghcr.io/hostinger`).

## Deploying on podbox

`deploy/podbox/` has everything except secrets:

| File | Purpose |
|---|---|
| [`setup.sh`](deploy/podbox/setup.sh) | Firecracker, guest kernel, CNI, a dedicated containerd 1.7 and a RAM-backed (tmpfs) devmapper thin-pool, plus the `fireactions-thinpool`, `fireactions-containerd` and `fireactions` systemd units. Leaves Docker's containerd alone. |
| [`config.example.yaml`](deploy/podbox/config.example.yaml) | `/etc/fireactions/config.yaml` minus the GitHub App key. |

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o fireactions ./cmd/fireactions
scp fireactions deploy/podbox/setup.sh podbox:/tmp/
ssh podbox 'sudo install -m755 /tmp/fireactions /usr/local/bin/ && sudo bash /tmp/setup.sh'
# then write /etc/fireactions/config.yaml (0600) and: sudo systemctl enable --now fireactions
```

Workflows target the pool with `runs-on: [self-hosted, fireactions-4vcpu-8gb]`.

### Fleet config (rendered)

Host configs are rendered from [`deploy/fleet.yaml`](deploy/fleet.yaml); don't hand-edit `/etc/fireactions/config.yaml`.

```bash
# secrets only from the environment (needed once a pool uses the cache):
export FLEET_CACHE_UPSTREAM_TOKEN=... FLEET_CACHE_TOKEN_RW=... FLEET_CACHE_TOKEN_RO=...
go run ./cmd/fireactions fleet render -f deploy/fleet.yaml -o rendered/   # rendered/ is gitignored
npm --prefix cache-proxy ci && npm --prefix cache-proxy run build         # only if the cache is enabled
deploy/fleet-apply.sh podbox rendered/podbox --dry-run                      # what would change
deploy/fleet-apply.sh podbox rendered/podbox                                # drains, installs, restarts
```

`fleet-apply.sh` restarts only units whose files changed, and drains every pool (waits for running jobs) before restarting fireactions. Adding a host is a `hosts:` entry plus `setup.sh` on the new machine.

### Runner image and rebuild policy

VMs boot [`images/ubuntu24.04`](images/ubuntu24.04/Dockerfile), published as `ghcr.io/junglem0nkey/fireactions-runner`: upstream's ubuntu24.04 image plus **our agent** (needed for per-pool `env`), the **latest `actions/runner`** and **Node 24**.

**Policy: every host runs an image built within 30 days of the latest `actions/runner` release.** GitHub stops sending jobs to runners more than 30 days out of date, and JIT runners can't opt out of updating, so a stale image re-downloads the runner on every VM boot and eventually gets no jobs at all. ([Scale-set](#fleet-config-rendered) runners register with `DisableUpdate`, so for them the image is the only way to update.)

- [`runner-image.yaml`](.github/workflows/runner-image.yaml) runs every Monday and on agent or image changes. When the runner, Node or agent changed, it pushes an immutable tag `ubuntu24.04-runner<ver>-node<ver>-<sha>` and opens a PR bumping `deploy/fleet.yaml`.
- Merge that PR and run `fleet-apply.sh` within the week. Pools pull `IfNotPresent`, so only a new tag reaches hosts.
- One-time setup: make the GHCR package public (Package settings → Change visibility), so hosts can pull it without credentials. Also enable *Settings → Actions → General → Allow GitHub Actions to create pull requests*.
- GitHub disables scheduled workflows after 60 days without repository activity. If the bump PRs stop, check the Actions tab.

### Gotchas we hit

- **Don't run upstream `install.sh` on a Docker host.** It overwrites `/etc/containerd/config.toml` and the containerd unit, and runs `vgcreate` on a block device.
- **No `noapic` in `kernel_args`.** Firecracker ≥1.8 describes virtio-mmio devices via ACPI, and `noapic` leaves them without IRQs, so the VM crash-loops.
- **Guest kernel:** 5.10 and 6.1 are past Firecracker's end of support and the upstream-hosted kernels now 404. We use Firecracker's CI 6.18 build.
- **Subnet:** upstream's `192.168.128.0/24` collides with Docker bridges; we use `10.200.0.0/24`. VMs get real resolvers from `/run/systemd/resolve/resolv.conf`, not the `127.0.0.53` stub.
- **Upstream's runner image has no Node.** Ours does (see above). On the upstream image, add `actions/setup-node` to jobs that need it, otherwise Bun ends up running Node tooling.

---

[![Go Report Card](https://goreportcard.com/badge/github.com/hostinger/fireactions)](https://goreportcard.com/report/github.com/hostinger/fireactions)

![Banner](docs/img/banner_violet.png)

Fireactions is an orchestrator for GitHub runners. BYOM (Bring Your Own Metal) and run self-hosted GitHub runners in ephemeral, fast and secure [Firecracker](https://firecracker-microvm.github.io/) based virtual machines.

> [!IMPORTANT]
> There's been multiple improvements with a lot of breaking changes. The current stable version is **v2.0.0**. Please use this version for production environments.

<!--
https://excalidraw.com/#json=GrJMj6LLYt39mgC0me7Di,C65TV9FhicnxNKgPeRhi3A
sequenceDiagram
    autonumber
    participant Fireactions
    participant Configuration file (YAML)
    participant Pool(s)
    participant Firecracker VM with GitHub runner
    participant GitHub

    Fireactions->>Configuration file (YAML): Load pools
    Fireactions->>Pool(s): Start pool(s)
    loop Ensure min amount of GitHub runners every 1s
        Pool(s)->>GitHub: Create JIT GitHub runner token
        Pool(s)->>Firecracker VM with GitHub runner: Start Firecracker VM
        Firecracker VM with GitHub runner->>GitHub: Run GitHub workflow job
        Firecracker VM with GitHub runner->>Pool(s): Exit (on workflow job finish)
    end
    GitHub->>Fireactions: Scale pool on workflow_job event
-->
![Architecture](docs/img/architecture.png)

Several key features:

- **Scalable**

  Pool based scaling approach. Fireactions always ensures the minimum amount of GitHub runners in the pool.

- **Ephemeral**

  Each virtual machine is created from scratch and destroyed after the job is finished, no state is preserved between jobs, just like with GitHub hosted runners.

- **Customizable**

  Define job labels and customize virtual machine resources to fit Your needs.

## Quickstart

```bash
$ fireactions --help
BYOM (Bring Your Own Metal) and run self-hosted GitHub runners in ephemeral, fast and secure Firecracker based virtual machines.

Usage:
  fireactions [command]

Main application commands:
  server      Starts the server
  agent       Starts the agent and GitHub Actions runner inside the VM

Pool management commands:
  pools       Manage pools

Machine management commands:
  ps          List all running machines across all pools
  login       SSH into a running VM as root user
  logs        Stream logs from the fireactions-agent service inside a machine

Image management commands:
  image       Manage images

Additional Commands:
  version     Show version information
  help        Help about any command
  completion  Generate the autocompletion script for the specified shell

Flags:
  -h, --help      help for fireactions
  -v, --version   version for fireactions

Use "fireactions [command] --help" for more information about a command.
```

See the [User Guide](https://fireactions.io/latest/) for installation and configuration instructions.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for more information on how to contribute to Fireactions.

## License

See [LICENSE](LICENSE)
