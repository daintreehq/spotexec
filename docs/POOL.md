# Spot worker pools: design

A pool runner (`type = "aws-spot"`, or `type = "sim"` for local testing) is a
fleet of identical VMs: from 0 up to `max_instances`, or with that unset,
as many as demand and the account's Spot vCPU quota allow. Each VM offers `max_slots`
concurrent job slots. Nothing runs outside the VMs: every `sx` invocation
takes part in scheduling, and every VM shuts itself down.

```
             sx (agent 1)   sx (agent 2)   sx (agent 3) ...
                  │              │              │
                  └──── pool.json + pool.lock (local, flock) ────┐
                                                                  │
   EC2 API ◀── List / Launch / Terminate (tags: runner + owner)   │
                                                                  ▼
   ┌─ i-a  [slot1 ●][slot2 ●]   retained, busy       ◀── pack: retained VMs, the
   ├─ i-b  [slot1 ●][slot2 ○]   retained, busy           worktree's own or a busy one first
   ├─ i-c  [slot1 ○][slot2 ○]   standby, idle 3m ── watchdog ─▶ terminates (early drain: 2m idle)
   └─ (launching)               claimed by agent 3
```

## Scheduling (client-side, per command)

1. **Pool order.** The runners in the pool are tried in `priority` order
   (lowest first), without blocking: an `ssh` runner directly, a fleet
   through its known instances.
2. **Known instances.** The fleet's instance list is read from the local cache
   (`pool.json`). It is refreshed from the cloud API when older than 8s, and
   after any launch, successful or not. Running instances are tried in the
   order the [placement policy](#placement-the-retained-set) gives, with a
   non-blocking acquire. Before contacting a VM, the job reserves a slot
   there in the fleet's ledger (`sched.json`), so other `sx` processes see
   it as taken at once; a VM whose slots this machine's jobs already fill is
   skipped without an SSH round trip. The VM's helper still decides.
   - An instance that refuses with `RUNNER_DRAINING` is marked draining and
     skipped.
   - An instance that no longer satisfies the runner's current requirements
     (its type left `instance_types`, or no longer meets `vcpus`,
     `min_memory_gib`, `min_generation` or `exclude_instance_types`, or its
     region left `regions`, after a config change) is skipped and stops counting as capacity, so waiting
     jobs get a matching replacement and it idles out. Unlike draining this
     is judged again whenever the config changes: reverting the change puts
     it back in service. A failed type lookup never retires an instance.
     `spotexec status` shows it as `outdated`, with the reason. `exec`,
     `pull` and `--same-slot`, which name an exact slot, still reach it.
     The pool manager keeps such a VM out of the warm pool, and picks up a
     change to the requirements when it reloads the edited config.
   - An instance that cannot be reached stays in the cache and still counts
     as capacity, but isn't probed again for 3s, or for 30s once it is
     older than `boot_timeout` and has failed to answer twice in a row.
   - When none of a region's VMs answered its last probe, and either two or
     more past `boot_timeout` have failed twice in a row or one has never
     answered since its launch, the region is held as **unreachable from
     this client** (the cloud's own health checks may still pass: it is
     this machine's network path that failed). New launches skip it until
     any VM there answers again. A hold with no answer lapses after 10
     minutes; one VM may then launch there to try it, and if that one never
     answers either the region is held again, twice as long (up to an
     hour). A never-reached VM in a held region stops covering waiting
     jobs at once, so their launch goes elsewhere. `spotexec status` and
     `spotexec prices` say which regions are held and why.
3. **Prepare beside preparing jobs.** A job holds its slot's command
   capacity only while its `[setup] run` and command run; syncing, the
   image, the dependency install and the write-back take only its own
   token and workspace. So when every slot is taken but some by jobs still
   preparing (or writing back), the job asks those VMs to admit it beside
   them (`Acquire.Overflow`, up to twice `max_slots` jobs per VM): it
   prepares there and waits for a free slot to run its command. A VM
   whose slots all run commands (or whose admitted jobs wait to) refuses.
   Plain requests (older clients) still see a VM full at `max_slots` jobs.
4. **Queue and scale out.** If every slot is busy, the job takes a ticket in
   a fair local queue (priority, then arrival; `queue.go`) and registers as
   demand on each fleet (a waiter file under `waiters/`). Under `pool.lock`,
   `Plan` launches only while waiting demand exceeds the capacity already
   starting (the launch ledger in `pool.json`), up to `max_instances` when set.
   - A short burst waits for slots about to free rather than launch a VM
     that would serve only after it: while the jobs holding the fleet's
     ready VMs are predicted (by the lease model, below) to end within a
     launch's lead, and those releases cover the demand not already
     starting, `Plan` holds the launch. The lead is the median time recent
     launches took from being planned to their VM first serving (timed by
     `spotexec up`, or by runs that saw the VM still booting within the
     30s before it served, or that probed it without an answer from within
     30s of its launch until it served, never more than 30s apart), or two
     minutes until three launches have been timed, and never more than ten. The hold lasts only while such
     releases are still predicted and the job has waited less than the
     lead: a prediction that turns out overdue frees nothing, so the next
     round launches, and a job that has waited longer than the lead is
     sustained demand, and launches as before. A job whose end is unknown
     frees nothing here either, nor does a VM that is draining, outdated,
     suspect, fenced or still starting. The first slots to free on a VM
     whose helper admitted jobs beyond its slots (step 3) are theirs, so
     those releases free nothing for jobs still waiting to be admitted. While an exclusive job waits (it
     needs a whole VM to empty, which freeing slots don't show), the fleet
     doesn't hold at all. Held demand counts as covered, so lower-priority
     fleets don't launch for it either. The hold is a `scale_held` event
     (with the slots it waits for, `free_in_ms`, `lead_ms`, `lead_learned`
     and `waited_ms`) and the queue status gives its reason; `scale_out`
     carries the same `lead_ms`, `lead_learned` and `waited_ms`.
   - Lower-priority fleets only launch for demand the higher-priority ones
     can't cover.
   - Draining and outdated VMs don't count toward `max_instances`, but
     with one set, launches pause while twice `max_instances` VMs are
     alive, until some end; the wait reason counts the draining and outdated VMs holding them.
   - A fleet at `max_instances` with jobs waiting ends one VM at a time in
     a region held as unreachable (one that failed its last probes and
     has none of this machine's runs on it), so the launch can go to a
     region it can reach. It doesn't when every region is held: those VMs
     are how the region is seen to answer again.
   - Ledger entries record the launching PID, so a dead `sx` frees its
     reservation; an ambiguous launch (the request may have created an
     instance) keeps its reservation until listings can show it.
   - Where an `aws-spot` launch goes (region, zone, type) is the provider's
     ranking, by default the [expected cost of the run](aws.md#rank-by-the-cost-of-the-run);
     the launch's reason is reported on the `instance_launched` and
     `instance_running` events, with the `launch_id` of its `scale_out`.
   - A failed launch backs the fleet off for 20s.
   - A launch refused by the account's vCPU quota (not a capacity shortage)
     holds further launches until one of the fleet's VMs ends or five
     minutes pass, so queued jobs wait for the running VMs. With no VM
     running and no static runner in the pool, the job fails at once with
     the quota increase command.
   - A launch that finds every Spot price above `max_price` holds back
     launches for the jobs already waiting the same way, and with no VM
     running the job fails at once, naming the cap and the cheapest
     current price. A job that starts later checks the prices again.
5. **Wait.** Tickets near the head of the queue probe instances about every
   0.5-2.5s (the head most often) until a slot frees or `queue_timeout`
   expires.

## Placement: the retained set

With `scheduling.policy = "consolidate"` (the default), placement answers two
questions separately.

**Which VMs are worth keeping?** Every `sx` process (and the pool manager)
runs a small controller under `pool.lock`, at most every 10 seconds. It
tracks how many slots this machine's jobs hold or have reserved, and keeps
a *retained set* big enough for the 90th percentile of that over the last
minute (time-weighted, so a burst of short jobs counts for its duration, not
its job count), plus `spare_slots`. When the retained VMs have had spare
capacity for 60 seconds, it moves one of them to *standby*, then at most one
more every 10 seconds while the surplus lasts:

- an empty VM goes before a busy one, then one whose worktrees' files other
  retained VMs also hold, a dearer one (when prices are known), one nearer
  `max_lifetime`, and the one used least recently;
- never a VM that joined in the last minute, one with an acquire in flight,
  an `exec`/`pull`/`--same-slot` in progress or an exclusive job's fence,
  nor one of the `min_instances` warm VMs;
- nothing shrinks while jobs are queued, or an exclusive job waits.

A standby VM's running jobs finish, new jobs go elsewhere, and nothing renews
its idle clock. Once this machine has nothing left on it, it is handed an
[early drain](#early-drain) and ends after `result_retention` (2m) idle;
otherwise its watchdog ends it after `idle_timeout`. Until that drain is
handed over, standby is reversible: when the retained VMs are full, the
standby VM that left most recently comes back before anything launches (and
stays for at least a minute). New VMs join the retained set.

**Where does this job go?** Among retained VMs with room: the one holding
this worktree's files (recently synced there), else this repository's
caches; then a busy VM before an idle one, the tightest fit, the most
recently used, and the longest-standing member. Then standby VMs, as above.
A worktree's last VM is a preference *inside* the retained set: it no longer
keeps a surplus VM alive by itself, so six worktrees whose jobs fit on three
VMs end up on three, not six.

`scheduling.mode = "latency"` lets a worktree go back to its own standby VM
before a retained VM without its files (keeping more VMs, for fewer cold
syncs).

**Cost-aware placement.** Once there is enough history, the retained VMs
with room are compared by what the job would cost on each, rather than by
the fixed order above alone:

    extra VM time it causes × the VM's price  +  its expected lease × wait_cost

The *extra VM time* is how much later the VM can idle out because of this
job. Reviving a VM that has sat idle for 9 of its 10 minutes costs the
job's whole lease plus those 9 minutes (its idle time already run down);
a busy VM whose jobs outlast this one costs nothing; a busy VM whose job
ends in a second costs nearly the whole lease. The *lease* (acquire to
release, sync and write-back included) is learned per task (repository and
command, hashed) and per how warm the VM is for it, so a cold sync shows up
as a longer lease: the last 16 leases each, for the 96 most recent tasks,
in `sched.json`. Prices are the VMs' current Spot prices, looked up at most
every 15 minutes for the whole machine (by `sx` in the background, and by
the pool manager), or the runner's `hourly_cost`. VMs within about a
second's worth of cost of the cheapest keep the fixed order between them,
and a VM whose `max_lifetime` would cut the job's slower leases short goes
last. Until every candidate is priced and the task (and the jobs already on
those VMs) has at least 3 leases, the fixed order stands: an unknown figure
is never taken as zero. The `placed` event's `score_mode` says which
(`measured` or `heuristic`), and `reason` why that VM.

**Exclusive jobs.** An exclusive job at the head of the queue that finds no
empty VM *fences* the retained VM with the fewest jobs: new shared jobs go
elsewhere, so it empties. Launches go on meanwhile, so it doesn't wait
behind a long job when a new VM would be ready sooner. The fence ends when
the job gets a VM, gives up, or its process dies.

**The ledger.** `sched.json`, beside `pool.json` in the runner's state
directory, records every job's claim from reservation to release: *reserved*
(nothing sent yet), *sent* (just before the acquire goes out), *acquired*,
or *uncertain*. Each `sx` process holds a lock on its own file under
`owners/`, so a dead process is told from a live one. A dead process's
reservation that was never sent is dropped; one that may have reached the VM
counts as uncertain until the VM's holder list shows it gone or six minutes
pass (the helper drops a silent client within that). Uncertain slots never
stop a job from asking the VM; they only keep the VM from being let go.
`exec`, `pull` and `--same-slot` are recorded too, but always go to their
exact slot, standby or not.

`policy = "shadow"` places as before but reports what the retained-set policy
would have chosen (`placement_shadow` events); `policy = "legacy"` is the
pre-0.10 placement: the oldest VM with a free slot, or the worktree's VM
while its last run is under 10 minutes old.

## Early drain

A standby VM ends once it has been idle for `scheduling.result_retention`
(default `2m`, never longer than `idle_timeout`), rather than at
`idle_timeout`. Retained VMs keep the full `idle_timeout`. This is on by
default and needs no [pool manager](scaling.md#pool-manager-optional): each
`sx` hands out the drains due as its job ends, and each VM carries its own
out.

What it costs:

- **Results.** Once a drained VM ends, `exec`, `pull` and `--same-slot` can
  no longer reach its workspaces. `result_retention` is how long after its
  last job they still can.
- **Relaunches.** Once its drain is committed, no new job is placed on the
  VM, even while it is still up keeping its results (`exec`, `pull` and
  `--same-slot` still reach their exact slot until it ends). If demand comes
  back, new jobs go to the retained VMs if they have room, and otherwise
  wait for a new VM to boot.

To keep surplus VMs until `idle_timeout` instead:

```toml
[runners.aws.scheduling]
early_drain = false
```

For a standby VM that has been standby for 30 seconds, isn't warm, holds
nothing of this machine's (no job, reservation, uncertain attempt or
exclusive fence), with no job queued and the retained VMs covering recent
demand without it, `sx` (as a job's lease ends) or the pool manager (each
probe pass):

1. records the drain in the ledger (`drain_requested`). From then on the VM
   is never placed on again, even if the request below times out, so a
   delayed request can't drain a VM that just took new work;
2. hands it to the VM (`spotexec-helper schedule-drain`, over the helper
   this machine already installed: installing one would touch its idle
   clock). The helper writes `drain_after` beside the watchdog's other
   files, and from then on the watchdog ends the VM once it has been idle
   for `result_retention` with no session holding its lifecycle lock, as it
   would at `idle_timeout`: it writes the drain marker first, so nothing can
   start after. Nothing on this machine has to keep running for it;
3. records the answer (`drain_scheduled`), and the VM isn't asked again.

A VM booted by an older spotexec has a watchdog that doesn't read
`drain_after`. It is asked to drain now instead (`try-drain`): the helper
takes its lifecycle lock exclusively without waiting, and only with no
session holding it, no run waiting for a workspace slot, and its activity
at least `result_retention` old (by the VM's own clock) writes its drain
marker. Busy or still keeping results, it is asked again 30 seconds later,
by the next `sx` to finish a job or the next pool manager pass, with the
same operation ID; with neither, it ends at `idle_timeout`.

Only VMs the placement policy has marked standby are drained: without the
pool manager, nothing re-sorts the retained set after the last `sx` exits,
so VMs still retained then end at `idle_timeout`. A Spot interruption or `max_lifetime` still
ends a VM whatever its role.

## Scale in

Each VM's watchdog (a systemd timer installed by cloud-init) powers it off
after `idle_timeout` (default 10m) without activity, or a standby VM after
`result_retention` (default 2m) once it has an [early drain](#early-drain).
Standby VMs get no new work, so this is how the surplus the controller found
ends. It only does so when no
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

On `spot_interruption` the CLI records that the VM's zone and instance type
were reclaimed (new launches rank them last for a while; the pool manager
does the same for notices it finds on idle VMs) and immediately checks for
**replacement capacity** in the background, excluding the dying VM: it probes for a free
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
once, if the VM is gone by then, or if the command hadn't been handed to the
VM yet (it was still syncing, building the image or installing dependencies).
A VM whose last heartbeats showed it out of memory is not, and nor is one that
EC2 still reports running after the command was handed over: the command may
be what overloaded it, and replayed it could take another VM's jobs down with
it (`MEMORY_PRESSURE`, `RUNNER_UNRESPONSIVE`; exit 125). A VM that announced it
was going away (a Spot notice, or the helper reporting its shutdown, as at
`max_lifetime`) is replayed either way. Before giving up on a
silent helper whose VM is still running, the CLI waits 3 more minutes, so a
VM that recovers keeps its runs.

A VM that goes silent with its whole region is not the run's doing. When the
CLI gives up on a running VM after handing it the command, it probes over SSH,
each on a connection of its own: the lost VM, the fleet's other VMs in that
region that this machine has reached before and still uses (up to 6), and one
VM in each of up to 3 other regions. If the lost VM and those peers (at least
one) all fail for want of a network path (a timeout, no route), and a VM in
another region answers, the region is taken to be unreachable from this
client (a network partition), and the run is replayed instead: on a VM already
up outside that region (it launches nothing, since a launch could go to the
silent region), and only after the longest a helper outlives the last word
from its client before it tears the run down and exits (about 6 minutes after
the loss), so the lost attempt isn't still running alongside the replay.
Memory pressure still rules a replay out, and so does a VM whose heartbeats
never reported its memory (`REGION_UNREACHABLE`; exit 125). A VM alone in its
region, a client that reaches no region, or a probe that fails for another
reason (authentication, say) proves nothing and keeps `RUNNER_UNRESPONSIVE`.

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
