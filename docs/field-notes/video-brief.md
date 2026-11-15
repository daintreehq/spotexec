# Video brief

Material for a video about spotexec, drawn from the Daintree trial. Every number here was measured during the trial unless it says otherwise.

## The story in one paragraph

Coding agents working in parallel worktrees all check their work on the same laptop, and the checks are the heavy part: test suites, type checks, linters, builds. spotexec moves just those commands onto Spot VMs that start when work queues up and power themselves off when it's done. The editor, the checkout and the agents stay local; output, exit codes and changed files come back as if the command ran locally. On Daintree, an Electron IDE with 114,000 unit tests, the full suite went from 15 to 20 minutes with the laptop locked up to under 11 minutes across four Spot VMs, with the laptop free.

## Numbers worth showing

| What | Number |
| :--- | :--- |
| Daintree unit suite | About 114,000 tests in about 5,000 files |
| Locally | 15 to 20 minutes, machine locked up (so agents were forbidden from running it) |
| Through spotexec | 10m37s, 9 parts across 4 Spot VMs, all green |
| Typecheck, lint and guards in parallel | 5m43s, each on its own VM |
| First sync | 13,551 files, 170 MB |
| Later syncs | A changed file: 1 file, 5 KB |
| A cold VM to first output | About 1m40s |
| One test file, warm | About 20 s (5 s of it spotexec and the test, the rest Daintree's workspace build) |
| Spot price of the VMs used | $0.129 to $0.33 an hour, cheapest chosen at each launch |
| A full-suite run | Roughly 15 to 25 cents of Spot time (an estimate from hourly prices) |

## Demo script

Warm the fleet first (one `sx` run a few minutes before recording), so the demo doesn't spend a minute and a half on a cold boot. Then:

1. **The problem.** Show the Daintree repo and say how big the suite is. Optionally start `npm test` locally and show the CPU meters pinned, then stop it.
2. **One command.** Run `sx test-all`. Show the parts starting, `spotexec status` in a second terminal with VMs and slots filling up, and the laptop's CPU staying idle.
3. **The result.** All shards green in about 11 minutes. Cut the wait in editing, and show the clock.
4. **Edits come back.** Run something that changes files, such as a formatter or `vitest -u`, and show the changes landing in the worktree, with a summary line saying what came back.
5. **Parallel checks.** `sx check`: typecheck, lint and guards on three VMs at once.
6. **Clean-up.** `spotexec status` after the idle timeout: the VMs power themselves off, and nothing is left running or billing.
7. **Agents.** An agent in a worktree runs `sx test-all` before opening its pull request. Several worktrees doing this at once is the case spotexec is built for.

## What makes it different

- **Nothing hosted.** No service, no control plane, no credentials on the VMs: coordination lives on the laptop and in EC2 tags.
- **Only the heavy commands move.** Unlike a cloud dev environment, the editor, agents and checkout stay where they are.
- **Only what changed travels**, and changed files come back through a three-way check that never overwrites an edit made during the run.
- **Jobs are isolated.** Each sees only its slot's CPUs, and one that runs out of memory is killed and reported on its own.
- **Cheapest by default.** Each VM launches on the cheapest instance type and zone available at that moment, and powers off when idle.

## The story behind it

The first real project turned up a run of problems in a day, some in spotexec and some in how the project was configured, and each was fixed and verified on real hardware the same day. That's a good story if it's told plainly:

- **Jobs that hurt each other.** One job's memory spike took a whole VM down, and then a second. Containers saw every CPU on the VM, so four test runs each started workers for eight cores.
- **Differences from a laptop or CI.** No `.git` on the runner, and `localhost` didn't resolve to IPv6.
- **A fix that broke something.** The first memory limit halved Node's default heap, which only a live run showed.

The point to make: the test suites and the simulator didn't find any of these. A real workload did, and that's why the project is being tried on real repositories before it's recommended widely.

## Caveats to say out loud

- **One project so far.** spotexec has been proven on one project: Node, Electron, AWS. Python, Go, Rust and the JVM are designed for but not yet tried on real repositories. SSH runners and macOS runners haven't been tried either.
- **Quick runs are slower remotely.** A single quick test takes longer remotely than locally, so the edit-and-rerun loop stays local.
- **Some suites need workarounds.** Suites that bind fixed ports, or start their own Docker containers, need `exclusive` tasks or have to stay local for now.
- **Not measured yet.** Today's numbers are for single runs. The multi-agent claim is the next thing to measure.
