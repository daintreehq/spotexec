# README suggestions

Changes the README would benefit from, based on the Daintree trial. Each says what is missing and why it matters to a first-time reader. None of them has been applied.

## Show a real result near the top

The README explains the idea well, but never shows a real project using it. One line under the opening paragraph would carry more weight than any feature list, for example:

> Daintree's 114,000-test suite runs in about 11 minutes across four Spot VMs, instead of 15 to 20 minutes with the laptop locked up.

The trial's numbers are in [the Daintree trial](daintree-trial.md#results).

## Add "one job can't take down the others" to "Why spotexec"

The current bullets cover output, sync, write-back, self-cleaning VMs and agents. They don't mention isolation, which the trial showed matters as much as speed when four jobs share a VM. A bullet such as:

> **Jobs stay out of each other's way.** Each job sees only its slot's CPUs, so test runners size themselves correctly, and a job that runs out of memory is killed and reported on its own, without taking down the VM or the jobs next to it.

The "VMs that clean up after themselves" bullet could also say that each VM launches on the cheapest instance type and zone available at the time.

## Set expectations for the first run

Quick start step 3 mentions the cold boot, but not the first dependency install, which for a real project is the larger cost (`npm ci` took over a minute of Daintree's first 1m49s run). A sentence saying the first run boots a VM, builds or pulls the image and installs dependencies once, and that later runs take seconds, would stop people judging spotexec on its slowest run.

## Say that `init` is a starting point

Step 2 reads as if `spotexec init` and `doctor` are all a project needs. For Daintree they weren't: the image needed system libraries and a locale, the cache had to match CI's, heavy tasks had to run `exclusive`, and every task needed a workspace build. One line pointing to a "tuning a real project" guide would help. [Tuning a real project](tuning-a-real-project.md) could be promoted into `docs/` for this, with its checklist trimmed to what applies to every project.

## Show a sharded task, not just its name

Step 3 lists `sx test-all # a sharded task`, but the configuration that makes it is in `docs/configuration.md`. Sharding the test suite over the fleet is the strongest demonstration of what spotexec is for, so a four-line example belongs in the README:

```toml
[tasks.test-all]
run = "npx vitest run --shard={shard}/{shards} {args}"
shards = 8
```

## Mention `git_seed` where people will hit its absence

A tool that asks git about the checkout fails remotely without `git_seed`, and the error (`not a git repository`, a null commit SHA, `git ls-files` failing) doesn't point to spotexec. Two of the trial's failures were this. The project config example in Quick start, or a note in "Your command, unchanged", could mention it.

## Offer the explicit route for agents, not only the hook

"With coding agents" goes straight to installing the hook. Some teams won't want commands rewritten behind an agent's back; Daintree chose explicit instructions instead. A short alternative helps them: a few lines for `CLAUDE.md` or `AGENTS.md` naming which commands go through `sx` (the whole suite, typecheck, lint) and which stay local (single test files while iterating), with the reason. The table in [Where it fits](where-it-fits.md#with-coding-agents) is a starting point.

## Say when not to use it

A short "When it doesn't help" note would build trust, and stop people from timing a 2-second test remotely and concluding it's slow. It should cover:

- **Quick single tests:** about 5 seconds a run even when warm.
- **Watch modes and dev servers:** these stay local.
- **Tests needing fixed ports or Docker:** until #79 and #80 are resolved.

## Give a sense of cost

The README says VMs scale to zero, but not what a run costs. A sentence with an order of magnitude ("a full run of a large suite across four 8-vCPU Spot VMs costs a few tens of cents") answers the first question most people will have. See [Costs](where-it-fits.md#costs) for how that figure was estimated, and replace it with a measured bill when there is one.
