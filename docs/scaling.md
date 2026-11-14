# Scaling and the pool manager
## Scaling, queueing and CPU sizing

Every `sx` process schedules for itself, against shared local state:

- **Placement.** Static (`ssh`) runners are tried first, by priority. On a fleet, a job takes a free slot on the **oldest** instance that has one. Packing work onto old VMs lets the newest ones go idle and scale in, with no coordinator.
- **Queueing.** When every slot is busy, the job takes a ticket in a fair local queue: priority first, then arrival. Spinners show `queued #3 of 9`, and agents get `queue_position` events.
- **Scaling out.** A queued job registers demand, and the fleet launches while waiting demand exceeds the capacity already booting, up to `max_instances`. Lower-priority fleets only launch for demand the higher-priority ones can't cover. A failed launch backs the fleet off for 20s. A launch the account's vCPU quota refuses is different: the fleet stops launching and runs queue for its running VMs until one ends (or five minutes pass), and with no VM running the run fails at once with the command to raise the quota.
- **Scaling in.** Each VM powers itself off after `idle_timeout`. Nothing has to tell it to.
- **CPU sizing.** Point your test runner at `$SPOTEXEC_CPUS` and it uses its fair share of the VM:

  ```sh
  vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS   # Vitest 2 needs --minWorkers=1 on small shares
  jest --maxWorkers=$SPOTEXEC_CPUS
  pytest -n $SPOTEXEC_CPUS
  go test -p $SPOTEXEC_CPUS ./...
  ```

To mix machine sizes, define two fleets: a 2-vCPU fleet for light jobs and a 4-vCPU fleet for heavy ones. Route tasks to them with `pool`, or let overflow spill from one to the other by priority. [docs/POOL.md](POOL.md) has the full scheduling design.

## Pool manager (optional)

`spotexec daemon` is a background process on **your** machine, not in the cloud. It works on the same local state as `sx`, so the two never double-launch, and `sx` behaves the same whether or not the daemon runs. It manages the fleets in `~/.config/spotexec/config.toml`, not project-defined ones. Restart it after editing that file.

```sh
spotexec daemon start      # background; log at ~/.local/state/spotexec/daemon/daemon.log
spotexec daemon status     # instances, health, warm set, recent actions (--json)
spotexec daemon stop       # instances keep their own idle timers
spotexec daemon run        # foreground, for launchd/systemd or debugging
```

What it adds:

- **Warm pools.** It keeps `min_instances` per fleet running and keeps their idle clocks fresh, so the first job of the day doesn't wait for a boot. About 15 minutes before a warm VM's `max_lifetime`, it drains that VM and boots its replacement, so jobs never meet the hard cap.
- **Faster interruption recovery.** It probes every instance every 30s. A VM with a Spot notice is drained, and its replacement launches during the 2-minute notice, not after the VM is gone.
- **Health replacement.** A VM that stops answering for 8 minutes, or never finishes first-boot setup, is terminated and replaced. If *no* VM answers, the problem is probably your network, so it replaces at most one every 30 minutes. A fleet whose new VMs fail to come up three times in an hour is paused for an hour.
- **Reaping.** It terminates VMs past `max_lifetime` (in case a watchdog failed), VMs stuck without an address, and leftover image-bake builders.

A warm VM costs its Spot price for as long as the daemon runs. Stopping the daemon terminates nothing: warm VMs idle out after `idle_timeout` like any other.
