# Spike: runner scale sets on podbox (#10)

Status: **runbook ready, not run yet.** Fill in the Results section and the go/no-go, then link this file from #10.

The spike uses the real scale-set code from #14 (`server/scaleset.go`) plus `hack/scaleset-probe` for the questions that don't need real jobs. Timebox: about one day, and most of that is waiting for jobs.

## Before you start (needs the user)

1. Create a throwaway **private** repo, e.g. `JungleM0nkey/firerunners-spike`. Never use a public repo: the probe and fireactions both refuse public repos.
2. Install the `apehost-fireactions` App on it, with the same permissions as on the dashboard repo.
3. Copy [`spike/fanout.yml`](spike/fanout.yml) into the spike repo as `.github/workflows/fanout.yml`.

## Setup on podbox (~15 min)

The spike runs inside the production fireactions process, next to the dashboard pool. Don't run a second fireactions instance: both would hand out vsock CIDs from 3 and collide.

1. Build and install this branch. It's backward compatible, so the dashboard pool keeps working.
   ```sh
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/fireactions
   scp fireactions podbox:/tmp/ && ssh podbox 'sudo install -m755 /tmp/fireactions /usr/local/bin/fireactions'
   ```
2. Add two small scale-set pools to `/etc/fireactions/config.yaml`. They simulate two hosts: separate scale sets that share one label.
   ```yaml
   - name: spike-a
     min: 0
     max: 1
     scale_set: { name: spike-a, labels: [fireactions-spike] }
     env: { SPIKE_POOL: a }
     runner:
       name: fa-spike-a
       image: ghcr.io/hostinger/fireactions-images/ubuntu24.04:20260901114140
       image_pull_policy: IfNotPresent
       group_id: 1
       organization: JungleM0nkey
       repository: firerunners-spike
       labels: [fireactions-spike]
     firecracker:   # copy binary_path/kernel_image_path/kernel_args from the dashboard pool
       machine_config: { vcpu_count: 2, mem_size_mib: 2048 }
   - name: spike-b   # identical, with b instead of a
   ```
3. `sudo systemctl restart fireactions && journalctl -fu fireactions`. Expect `Scale set N session open, max capacity 1` for both pools.
4. Check the dashboard pool still works: push a dashboard PR or re-run its CI. This also covers the "podbox runs unchanged" checks for #7, #8 and #12.

## Questions

Build the probe locally and run it on podbox. It reads the App key, so run it as root.

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o scaleset-probe ./hack/scaleset-probe
scp scaleset-probe podbox:/tmp/
# on podbox:
P="/tmp/scaleset-probe -repo JungleM0nkey/firerunners-spike -app-id 5235650 -key /etc/fireactions/app.pem"
```

| # | Question | How |
|---|---|---|
| Q1 | Do two scale sets (one per host) sharing a label both receive jobs? | Run `fanout` (4 jobs). In the journal, look for `Scale set assigned jobs` lines on **both** `spike-a` and `spike-b`. The job logs print `$RUNNER_NAME` (`fa-spike-a-…` / `fa-spike-b-…`) and `SPIKE_POOL`. With max 1 each, all 4 jobs should finish, two at a time. |
| Q2 | Can two message sessions be open on one scale set at once? | While fireactions holds its session: `sudo $P session spike-a`. `REFUSED` or `OPENED` is the answer. |
| Q3 | Is `self-hosted` accepted as a scale-set label? | `sudo $P labels spike-label-test self-hosted fireactions-spike`. Is `self-hosted` in the kept list? Then try a job with `runs-on: [self-hosted, fireactions-spike]`. |
| Q4 | Does the default runner-group lookup work for a personal-account repo? | `sudo $P group`. Separately, step 3 of setup registers with the hard-coded group ID 1, which is what fireactions uses. |
| Q5 | Do scale-set calls count against the REST rate limit? | `sudo $P ratelimit`, leave the pools idle for 30 minutes, `sudo $P ratelimit` again. fireactions itself makes about one REST call per VM. If `remaining` barely moves while about 36 long polls happen, the answer is no. |

Also record:

- Whether a JIT VM from a scale set picks up its job, and that `JobStarted` / `JobCompleted` lines appear.
- What happens to the listener after `sudo ip link set <uplink> down; sleep 60; ... up`, or a `systemctl restart` of the network. Expect `reconnecting in …` with backoff, then a new session, without restarting fireactions (#14 acceptance).
- Lowering demand mid-job: with a long-running fanout job on a warm VM, the pool must not stop it (#12).

## Cleanup

Remove the spike pools from the config and restart fireactions. Then delete the two scale sets: they stay registered after their pools are gone. Use the repo's Settings → Actions → Runners page, or `DELETE /repos/OWNER/REPO/actions/runners/...`. Uninstall the App from the spike repo, or delete the repo.

## Results

_Date:_ …  _fireactions commit:_ …

| # | Answer | Evidence (journal lines, job links) |
|---|---|---|
| Q1 | | |
| Q2 | | |
| Q3 | | |
| Q4 | | |
| Q5 | | |

## Go / no-go

- **Go** if Q1 is yes: one scale set per host, as built in #14. If Q3 is no, drop `self-hosted` from the scale-set labels. Workflows target `[fireactions-4vcpu-8gb]` instead, which is a one-line change in the dashboard workflows.
- **No-go** if Q1 is no and Q2 is no. Open the fallback ticket: webhook plus hosted scheduler Durable Object, with HTTP control endpoints on the metrics mux (research §1.4 (a)).
- If Q1 is no but Q2 is yes: hosts could share one scale set, each with its own session. But then `X-ScaleSetMaxCapacity` is per session, so check that GitHub sums it before going with this.
