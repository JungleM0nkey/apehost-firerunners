# podbox memory budget (#11)

Status: **runbook ready, not measured yet.** Fill in Results, then set the numbers in `fleet.yaml` (#16) and link this file from #11.

Everything that competes for podbox RAM:

| Consumer | Bound today | Notes |
|---|---|---|
| Guest RAM | `mem_size_mib` × VMs (2 × 8 GiB) | Faulted in on demand and never returned: there's no balloon. So it trends to the peak the guest ever touched. |
| Thin-pool tmpfs | `size=36G` (setup.sh) | Only pages written to the loop files. `discard_blocks = true` should hand freed blocks back when a snapshot is removed. Measure whether it does. |
| Cache proxy tmpfs | the cap we're choosing (#13/#15) | The artifacts dir plus the Durable Object dir. |
| Firecracker VMM overhead | ≤ 5 MiB per VM | Per the Firecracker spec. |
| Everything else | Docker, `windows-test`, Forgejo, … | Measured as idle `MemAvailable`. |

## Measure (~1 hour, mostly waiting)

The script is read-only. Copy it over first: `scp deploy/podbox/measure.sh podbox:/tmp/`.

1. **Idle baseline.** Pause the dashboard pool so no VMs run (`fireactions pools pause fireactions-4vcpu-8gb -e 127.0.0.1:18080`). Wait for the VMs to exit, then:
   `sudo /tmp/measure.sh snapshot > idle.csv`
2. **Dashboard build peak.** Resume the pool, then start sampling and trigger the dashboard CI (both jobs, ideally two runs in parallel):
   `sudo /tmp/measure.sh watch 2 > build.csv` (Ctrl-C when CI is done).
   The peak `fc_rss_max` is the per-VM peak; the minimum `mem_available` is the floor.
3. **Thin-pool give-back, about 20 VM cycles.** With the pool at its normal size, run
   `sudo /tmp/measure.sh cycles 20 > cycles.csv`
   while re-running dashboard CI until 20 VMs have exited. Compare the first and last `thinpool_tmpfs_used`. If it keeps rising with every cycle, freed blocks are not returned through the loop device. In that case the tmpfs only grows until the thin-pool is recreated, so its full `size=` counts against the budget.

## Sizing formula

```
budget        = idle mem_available − headroom (2 GiB)
per_vm        = peak fc_rss_max (≈ guest RAM actually touched) + 5 MiB
thinpool      = last thinpool_tmpfs_used after 20 cycles (or 36 GiB if it never shrinks)
max_vms       = floor((budget − thinpool − cache_cap) / per_vm)
cache_cap     = what's left, rounded down; keep ≥ 2 GiB or the proxy isn't worth it
```

Pick `max_vms` first: that's the pool's `max` and the capacity advertised to GitHub (#14). Then give the rest to `cache_cap`, the proxy's `max_bytes` and its tmpfs `size=`.

## Results

_Date:_ …  _Load:_ …

| Measurement | Value |
|---|---|
| MemTotal | |
| Idle MemAvailable | |
| Min MemAvailable during build | |
| Peak Firecracker RSS per VM | |
| Thin-pool tmpfs used: before / after 20 cycles | |
| Freed blocks returned? | |

## Proposal

- `max` VMs per host (podbox): …
- Cache cap: …
- Thin-pool tmpfs `size=`: …
