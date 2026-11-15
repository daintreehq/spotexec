# Spot worker pools: design

A pool runner (`type = "aws-spot"`, or `type = "sim"` for local testing) is a
fleet of 0 to `max_instances` identical VMs. Each VM offers `max_slots`
concurrent job slots. Nothing runs outside the VMs: every `sx` invocation
takes part in scheduling, and every VM shuts itself down.

```
             sx (agent 1)   sx (agent 2)   sx (agent 3) ...
                  │              │              │
                  └──── pool.json + pool.lock (local, flock) ────┐
                                                                  │
   EC2 API ◀── List / Launch / Terminate (tags: runner + owner)   │
                                                                  ▼
   ┌─ i-a  [slot1 ●][slot2 ●]   busy                 ◀── pack: the oldest instance
   ├─ i-b  [slot1 ●][slot2 ○]   busy                     with a free slot first
   ├─ i-c  [slot1 ○][slot2 ○]   idle 3m ── watchdog ─▶ terminates at idle_timeout (10m)
   └─ (launching)               claimed by agent 3
```

## Scheduling (client-side, per command)

1. **Pool order.** The runners in the pool are tried in `priority` order
   (lowest first), without blocking: an `ssh` runner directly, a fleet
   through its known instances.
2. **Known instances.** The fleet's instance list is read from the local cache
   (`pool.json`). It is refreshed from the cloud API when older than 8s, and
   after any launch, successful or not. Running instances are tried oldest
   first (ties broken by instance ID) with a non-blocking acquire.
   - Packing work onto the oldest instances lets the newest go idle and
     terminate: the pool scales in without any coordinator.
   - While the worktree's last-run record (the slot `--same-slot` returns
     to) is less than 10 minutes old, the instance it names is tried first
     instead, since its slot still holds the worktree's files and build
     caches. A run is recorded once its files are synced and its container
     image is ready; a parallel or sharded task, once it finishes; a `--sync none` run,
     never. After that, work goes back to the oldest instances.
   - An instance that refuses with `RUNNER_DRAINING` is marked draining and
     skipped.
   - An instance that no longer satisfies the runner's current requirements
     (its type left `instance_types`, or no longer meets `vcpus`,
     `min_memory_gib`, `min_generation` or `exclude_instance_types` after a
     config change) is skipped and stops counting as capacity, so waiting
     jobs get a matching replacement and it idles out. Unlike draining this
     is judged again whenever the config changes: reverting the change puts
     it back in service. A failed type lookup never retires an instance.
     `spotexec status` shows it as `outdated`, with the reason. `exec`,
     `pull` and `--same-slot`, which name an exact slot, still reach it.
     The pool manager keeps such a VM out of the warm pool, but it judges
     by the config it started with: restart it after changing requirements.
   - An instance that cannot be reached stays in the cache and still counts
     as capacity, but isn't probed again for 3s, or for 30s once it is
     older than `boot_timeout` and has failed to answer twice in a row.
3. **Queue and scale out.** If every slot is busy, the job takes a ticket in
   a fair local queue (priority, then arrival; `queue.go`) and registers as
   demand on each fleet (a waiter file under `waiters/`). Under `pool.lock`,
   `Plan` launches only while waiting demand exceeds the capacity already
   starting (the launch ledger in `pool.json`), up to `max_instances`.
   - Lower-priority fleets only launch for demand the higher-priority ones
     can't cover.
   - Ledger entries record the launching PID, so a dead `sx` frees its
     reservation; an ambiguous launch (the request may have created an
     instance) keeps its reservation until listings can show it.
   - A failed launch backs the fleet off for 20s.
   - A launch refused by the account's vCPU quota (not a capacity shortage)
     holds further launches until one of the fleet's VMs ends or five
     minutes pass, so queued jobs wait for the running VMs. With no VM
     running and no static runner in the pool, the job fails at once with
     the quota increase command.
4. **Wait.** Tickets near the head of the queue probe instances about every
   0.5-2.5s (the head most often) until a slot frees or `queue_timeout`
   expires.

## Scale in

Each VM's watchdog (a systemd timer installed by cloud-init) powers it off
after `idle_timeout` (default 10m) without activity. It only does so when no
session holds the lifecycle lock. Because instances launch with
`InstanceInitiatedShutdownBehavior=terminate`, powering off terminates the
instance.

A draining VM with no sessions shuts down at once, without waiting for
`idle_timeout`. `max_lifetime` is a hard cap. So it never cuts a run short
that started late, a VM stops taking new runs 15 minutes before it (a
quarter of a shorter `max_lifetime`): every `sx` marks it draining in
`pool.json`, so it no longer counts as capacity and waiting jobs get a
replacement, and the VM's helper refuses new runs from then on (including
`--same-slot` and `exec`; `pull` still works). This holds with or without
the pool manager.

## Interruptions

The helper polls IMDSv2 every 5s for the whole session:

- `events/recommendations/rebalance` (an early, advisory warning): recorded
  and reported as a `spot_rebalance` event only. Real EC2 often sends one
  the moment an instance boots in a capacity pool at elevated risk, so
  draining on it would make the fleet churn.
- `spot/instance-action` (about 2 minutes' notice): the helper marks the VM
  draining and sends `spot_interruption` to the CLI.

On `spot_interruption` the CLI immediately checks for **replacement
capacity** in the background, excluding the dying VM: it probes for a free
slot (without keeping it) and registers demand, so a launch starts if one is
needed. The optional pool manager (`spotexec daemon`) also replaces a
noticed warm instance within the notice. Then:

- **The job finishes on the dying VM:** the result is used normally.
- **The VM dies first (connection lost), or its shutdown kills the
  command:** the whole run (sync, deps, exec, write-back) is replayed on the
  replacement slot. Nothing was written back locally from the lost attempt,
  so a replay is safe for anything that is safe to re-run.

There is at most one reallocation per run by default (`--no-retry` disables
it). A connection lost without a notice on an ephemeral runner is also retried
once, if the VM is gone by then. A VM that EC2 still reports running, or whose
last heartbeats showed it out of memory, is not: the run may be what
overloaded it, and replayed it could take another VM's jobs down with it
(`RUNNER_UNRESPONSIVE`, `MEMORY_PRESSURE`; exit 125). A VM that announced it
was going away (a Spot notice, or the helper reporting its shutdown, as at
`max_lifetime`) is replayed either way. Before giving up on a
silent helper whose VM is still running, the CLI waits 3 more minutes, so a
VM that recovers keeps its runs.

## Simulation (`type = "sim"`)

The `sim` provider implements the same fleet contract with Docker containers
on the local machine:

| Cloud concept | Simulation |
| --- | --- |
| Instance | A container labelled with the runner and owner, running sshd |
| Boot | The same cloud-init user-data. `systemctl` is stubbed and the watchdog runs in a loop every 2s; `boot_delay` adds launch latency. |
| Self-termination | The watchdog's self-destruct kills PID 1, so the container exits and is removed (`--rm`) |
| Spot notice | `spotexec sim interrupt` writes `/var/tmp/spotexec/sim/instance-action` (the helper checks it before IMDS), then kills the container after `--after` (default 10s) |
| Rebalance | `spotexec sim rebalance` writes `/var/tmp/spotexec/sim/rebalance` (advisory only) |

`scripts/e2e-pool.sh` drives the scenarios below against the simulator:
- scale out to `max_instances`;
- bin-packing onto busy instances;
- queueing at capacity;
- idle scale-in to zero;
- reallocation after an interruption;
- rebalance recommendations staying advisory.
