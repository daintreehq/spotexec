# Scaling and the pool manager
## Scaling, queueing and CPU sizing

Every `sx` process schedules for itself, against shared local state:

- **Placement.** Runners are tried in `priority` order, lowest first, whether they are `ssh` machines or fleets. On a fleet, a job takes a free slot on the **oldest** instance that has one. Packing work onto old VMs lets the newest ones go idle and scale in, with no coordinator. The exception is a warm VM: if a worktree's last run got its files synced less than 10 minutes ago, the worktree's next run tries that run's VM first, since its slot still holds the worktree's files and build caches. After that, work consolidates onto the oldest VMs again. A VM close to its `max_lifetime` takes no new runs at all (see *Retiring* below).
- **Queueing.** When every slot is busy, the job takes a ticket in a fair local queue: priority first, then arrival. Spinners show `queued #3 of 9`, and agents get `queue_position` events.
- **Scaling out.** A queued job registers demand, and the fleet launches while waiting demand exceeds the capacity already booting, up to `max_instances`. Lower-priority fleets only launch for demand the higher-priority ones can't cover. A failed launch backs the fleet off for 20s. A launch the account's vCPU quota refuses is different: the fleet stops launching and runs queue for its running VMs until one ends (or five minutes pass), and with no VM running the run fails at once with the command to raise the quota.
- **Memory.** Each job's fair share of the VM's memory is `$SPOTEXEC_MEMORY` MiB, after a reserve for the runner itself; size heaps and worker counts from it. In project containers on spotexec's VMs, jobs sharing a VM are capped together at the VM's memory less the reserve, so they can burst, and if one spikes past what's left, it is OOM-killed (`OOM_KILLED`) instead of exhausting the VM for every slot. Give memory-hungry jobs `--exclusive` or a fleet with fewer `max_slots`. A VM that stops answering while EC2 still reports it running gets 3 more minutes before its runs are given up on, and a run lost on a VM that was out of memory, or still running, isn't replayed elsewhere, unless the VM announced it was going away (a Spot notice, or the helper reporting its shutdown).
- **Scaling in.** Each VM powers itself off after `idle_timeout`. Nothing has to tell it to.
- **Retiring.** A VM stops taking new runs 15 minutes before its `max_lifetime` (a quarter of a shorter one), so the hard cap never interrupts a run that started after that point. Runs already on it carry on; longer ones can still meet the cap.
- **CPU sizing.** Each job's fair share of the VM is `$SPOTEXEC_CPUS` CPUs. In a [project container](environments.md#project-containers) on a Linux runner, the job is normally [pinned](environments.md#settings) to that many CPUs, so tools that size themselves by the CPUs they may use (`nproc`, Node's `os.availableParallelism()`, Go's `GOMAXPROCS`, and the test runners built on them) use its share by default. A pinned job can't borrow idle slots' cores; that's what `--exclusive` is for. Without a container, or where nothing is pinned (macOS runners, for one), nothing limits what a job sees, so point your test runner at `$SPOTEXEC_CPUS`:

  ```sh
  vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS   # Vitest 2 needs --minWorkers=1 on small shares
  jest --maxWorkers=$SPOTEXEC_CPUS
  pytest -n $SPOTEXEC_CPUS
  go test -p $SPOTEXEC_CPUS ./...
  ```
- **Sharding.** A suite too big for one slot can run as a [sharded task](configuration.md#tasks): `shards = 8` queues eight parts at once, so the fleet scales out for them like any other demand, and each part runs its `--shard={shard}/{shards}` slice.

### Several machines, one Spot quota

The Spot vCPU quota belongs to the AWS account and region, so every machine using the account draws on the same quota, while each plans its own fleet. Before each Spot launch, spotexec reads the quota (Service Quotas) and counts the vCPUs of every pending or running Spot instance in the region (EC2 `DescribeInstances`). It attributes them to this machine, to other spotexec machines (by their `spotexec-owner` and `spotexec-host` tags), or to Spot instances spotexec didn't launch. A launch must fit what's left, and the quota is shared like this:

- A machine with no VMs, or below its share, may use any room left. Its share is the quota divided among the machines that have VMs in the region, itself included.
- A machine at or above its share keeps growing only while room for one more VM remains, so it can't take the last room from the other machines.
- Nothing is reserved for machines with no VMs, and nothing is capped in the config: VMs idle out after `idle_timeout`, and their vCPUs go back to whoever needs them next.

When a launch doesn't fit, the machine behaves as for any [quota refusal](aws.md#5-check-your-spot-vcpu-quota): its runs queue for the VMs it already has, or, with none, fail at once with exit 125. The error shows who is using the quota, for example `16 of 16 vCPUs in use: 8 by this machine, 8 by studio-01`, and the command to raise it. `spotexec status -A` prints the same breakdown for every region your runners use. If the quota can't be read, spotexec launches anyway and lets EC2 decide. The check is a snapshot, so EC2 stays the final judge: two machines launching at the same moment can both pass it, and open Spot requests from other tools that have no instance yet aren't counted. EC2 then refuses the launch, which is handled the same way.

To mix machine sizes, define two fleets: a 2-vCPU fleet for light jobs and a 4-vCPU fleet for heavy ones. Route tasks to them with `pool`, or let overflow spill from one to the other by priority. [docs/POOL.md](POOL.md) has the full scheduling design.

### One fleet, several instance types

A fleet's VMs can be different instance types, because each VM goes to whichever of the runner's types is cheapest when it launches. Slots and CPU shares stay predictable as long as the types have the same vCPUs:
- **Slots** come from the VM itself: `max_slots`, or its vCPUs ÷ 4. The pool plans capacity with the runner's vCPUs (`vcpus`, or the first listed type's), so a list that mixes sizes (`c7a.xlarge` with `c7a.2xlarge`) makes plans that don't match every VM. Keep one size per runner, or set `max_slots`.
- **`$SPOTEXEC_CPUS`** is the VM's vCPUs ÷ its slots, so it is the same on every VM of one size.
- **Memory** is what differs. With `vcpus` and `min_memory_gib`, every VM has at least `min_memory_gib`, and some (memory-optimized types that happened to be cheapest) have more, so `$SPOTEXEC_MEMORY` can change from one launch to the next. It is the VM's memory less a reserve for the runner itself (1/16 of it, between 512 MiB and 2 GiB), ÷ slots, and the OS sees a little less than a type's nominal memory, so the least a job can count on is somewhat under `min_memory_gib ÷ slots`: with `min_memory_gib = 32` and `max_slots = 4`, about 7.3 GiB each. Size heaps and worker counts from `$SPOTEXEC_MEMORY` at run time rather than from the config, and leave headroom in `min_memory_gib` when jobs need a fixed amount.

That is why `vcpus` is an exact count rather than a range: one count means one slot layout across the fleet.

## Pool manager (optional)

`spotexec daemon` is a background process on **your** machine, not in the cloud. It works on the same local state as `sx`, so the two never double-launch, and `sx` behaves the same whether or not the daemon runs. It manages the fleets in `~/.config/spotexec/config.toml`, not project-defined ones. Restart it after editing that file.

```sh
spotexec daemon start      # background; log at ~/.local/state/spotexec/daemon/daemon.log
spotexec daemon status     # instances, health, warm set, recent actions (--json)
spotexec daemon stop       # instances keep their own idle timers
spotexec daemon run        # foreground, for launchd/systemd or debugging
```

What it adds:

- **Warm pools.** It keeps `min_instances` per fleet running and keeps their idle clocks fresh, so the first job of the day doesn't wait for a boot. About 25 minutes before a warm VM's `max_lifetime`, it drains that VM and boots its replacement, one VM at a time, so warm capacity stays up.
- **Faster interruption recovery.** It probes every instance every 30s. A VM with a Spot notice is drained, and its replacement launches during the 2-minute notice, not after the VM is gone.
- **Health replacement.** A VM that stops answering for 8 minutes, or never finishes first-boot setup, is terminated and replaced. If *no* VM answers, the problem is probably your network, so it replaces at most one every 30 minutes. A fleet whose new VMs fail to come up three times in an hour is paused for an hour.
- **Reaping.** It terminates VMs past `max_lifetime` (in case a watchdog failed), VMs stuck without an address, and leftover image-bake builders.

A warm VM costs its Spot price for as long as the daemon runs. Stopping the daemon terminates nothing: warm VMs idle out after `idle_timeout` like any other.
