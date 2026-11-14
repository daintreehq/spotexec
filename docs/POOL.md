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
   ┌─ i-a  [slot1 ●][slot2 ●]   busy                 ◀── bin-pack: fill the busiest
   ├─ i-b  [slot1 ●][slot2 ○]   busy                     instance with room first
   ├─ i-c  [slot1 ○][slot2 ○]   idle 3m ── watchdog ─▶ terminates at idle_timeout (5m)
   └─ (launching)               claimed by agent 3
```

## Scheduling (client-side, per command)

1. **Static runners first.** `ssh` runners in the pool are tried in priority
   order, without blocking.
2. **Known instances.** The fleet's instance list is read from the local cache
   (`pool.json`). It is refreshed from the cloud API when older than 8s, or
   after any failure. Running instances are tried oldest first (ties broken
   by instance ID) with a non-blocking acquire.
   - Packing work onto the oldest instances lets the newest go idle and
     terminate: the pool scales in without any coordinator.
   - An instance that refuses with `RUNNER_DRAINING` is marked draining and
     skipped.
   - An instance that cannot be reached is dropped from the cache.
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
`idle_timeout`. `max_lifetime` is a hard cap.

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
- **The VM dies first (connection lost):** the whole run (sync, deps, exec,
  write-back) is replayed on the replacement slot. Nothing was written back
  locally from the lost attempt, so a replay is safe for anything that is
  safe to re-run.

There is at most one reallocation per run by default (`--no-retry` disables
it). A connection lost without a notice on an ephemeral runner is also retried
once.

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
