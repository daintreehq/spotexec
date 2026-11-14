# spotexec

**Run your tests, type checks, linters and builds on remote hardware, straight from a local git worktree.**
Use a homelab box over SSH, or AWS Spot instances that boot on demand and terminate themselves when idle.
Files the command changes (`prettier --write`, `eslint --fix`, `vitest -u`, codegen) come back to your worktree through a 3-way check, so edits you make during the run are never overwritten.

Prefix any command with `sx` (short for `spotexec run --`):

```console
$ sx pnpm vitest run -u src/auth.test.ts
 ✓ src/auth.test.ts (12 tests) 340ms
● 4.1s on aws#3 · synced 4 files (18 KB) · pulled 2 modified files
```

`spotexec` is built for running many coding agents in parallel worktrees without melting your laptop. Many worktrees share a fleet of runners through isolated slots, a fair queue and a shared dependency cache. Nothing runs in the cloud except the VMs themselves. There is no control plane, no agent on your account and no credentials on the VMs.

- **Drop-in.** `sx <command>` runs any command remotely. Output streams back unchanged, exit codes are preserved, and remote paths in stack traces are rewritten to local ones.
- **Elastic.** AWS Spot fleets scale from zero to `max_instances` as demand queues up, pack jobs onto the oldest VMs, and scale back to zero on their own.
- **Safe.** Write-back compares content IDs against the exact bytes that were synced. A file you edited during the run is never overwritten: you keep it, and the runner's version is saved aside.
- **Fast.** Only missing files are sent. Dependencies restore from a lockfile-keyed cache (local reflink or copy, plus S3 across VMs), and baked AMIs skip first-boot setup.
- **Spot-aware.** Interruption notices are handled. A run that loses its VM is replayed once on a replacement, and an optional pool manager keeps warm capacity and replaces failing VMs.
- **Agent-friendly.** It emits NDJSON lifecycle events when it isn't on a TTY, and a Claude Code hook can move heavy Bash commands to the fleet transparently.

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [AWS setup](#aws-setup)
- [Running commands](#running-commands)
- [Using spotexec with coding agents](#using-spotexec-with-coding-agents)
- [How it works](#how-it-works)
- [Scaling, queueing and CPU sizing](#scaling-queueing-and-cpu-sizing)
- [The life of a Spot VM](#the-life-of-a-spot-vm)
- [Pool manager (optional)](#pool-manager-optional)
- [Dependency cache](#dependency-cache)
- [Baked images](#baked-images)
- [Project containers](#project-containers)
- [Write-back and artifacts](#write-back-and-artifacts)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Operations and troubleshooting](#operations-and-troubleshooting)
- [Command reference](#command-reference)
- [Files and state](#files-and-state)
- [Known limits](#known-limits)
- [Development](#development)
- [Design decisions](#design-decisions)

---

## Install

**Homebrew** (macOS and Linux):

```sh
brew install daintreehq/tap/spotexec     # installs spotexec, the sx alias and shell completions
spotexec version
```

**Debian / Ubuntu, Fedora / RHEL, Alpine:** download the `.deb`, `.rpm` or `.apk` for your architecture from the [latest release](https://github.com/daintreehq/spotexec/releases/latest), then:

```sh
sudo apt install ./spotexec_*_linux_amd64.deb   # or: sudo dnf install ./spotexec_*.rpm · sudo apk add --allow-untrusted ./spotexec_*.apk
```

**Any other system:** the release also has plain `.tar.gz` archives for macOS and Linux on amd64 and arm64. Each one holds a single `spotexec` binary; put it on your `PATH` and link `sx` to it.

**From source** (Go 1.26+):

```sh
git clone https://github.com/daintreehq/spotexec.git && cd spotexec
mkdir -p "$(go env GOPATH)/bin"   # make sure it exists and is on your PATH
make install        # builds bin/spotexec and the bin/sx symlink, installs both into $(go env GOPATH)/bin
```

`go install` isn't supported: the build cross-compiles the remote helper for Linux and macOS (amd64 and arm64) and embeds it in the `spotexec` binary, which `make build` and the release builds do. Nothing needs to be preinstalled on a runner: on first contact the helper uploads itself to `~/.cache/spotexec/bin/` there.

Your machine needs **`git`** and **OpenSSH** (`ssh`, used with ControlMaster) on `PATH`.

A runner needs:
- an SSH server, `bash` and `gzip`;
- Linux or macOS, on amd64 or arm64;
- Docker only if you use [project containers](#project-containers). On AWS, Docker is installed for you.

The AWS CLI v2 is **optional**: `spotexec` signs AWS requests itself (there is no AWS SDK in the binary). Install the CLI if you sign in with SSO, `aws login`, assume-role or `credential_process`. See [Credentials](#2-sign-in).

`sx` is a symlink to `spotexec`. `sx <command>` is shorthand for `spotexec run -- <command>`.

## Quick start

Terminology used throughout this README:
- A **runner** is either one **SSH host** or an elastic **fleet** of Spot **instances** (VMs).
- A **pool** is the ordered list of runners a command may use.
- Each machine offers `max_slots` isolated **slots**: one job per slot.

### 1. Describe your hardware (once per machine)

```sh
spotexec init --global      # writes an example ~/.config/spotexec/config.toml
```

Edit it so `default_pool` lists only runners you actually have. The template includes an `aws` runner.

**Option A: an SSH machine you already own.** Non-interactive key login must already work, and the host key must be in `~/.ssh/known_hosts`, because spotexec runs `ssh -o BatchMode=yes`. Check with `ssh -o BatchMode=yes me@192.168.1.50 true`. Commands run under `bash` with a login profile (`~/.bash_profile`, falling back to `~/.profile`; not `~/.bashrc` or `~/.zprofile`), so your toolchain (`pnpm`, `go`, `cargo`, …) must be on `PATH` there, set by `remote_path_setup`, or come from a [project container](#project-containers).

```toml
version = 1
default_pool = ["homelab"]

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]   # same machine, two routes
max_slots = 4
```

**Option B: AWS Spot, which scales to zero.** First work through [AWS setup](#aws-setup): credentials, region, IAM, Spot quota. VMs boot plain Ubuntu 24.04, so give them your toolchain. Either install it with `bootstrap_script` / `user_bootstrap_script` (see [step 7](#7-configure-the-runner)), or per repo with a [project container](#project-containers) (`spotexec container init`).

```toml
version = 1
default_pool = ["aws"]

[runners.aws]
type = "aws-spot"
profile = "spotexec"   # your AWS CLI profile (or omit to use AWS_PROFILE / env keys)
region = "us-east-2"   # always set it: the profile's region is not used
instance_types = ["c7a.xlarge", "c6a.xlarge"]   # tried in order on capacity errors
max_slots = 2          # concurrent jobs per VM
max_instances = 4      # fleet ceiling
idle_timeout = "10m"   # the VM powers itself off after this long without work
```

With both, `default_pool = ["homelab", "aws"]` plus `priority = 10` on `homelab` and `priority = 20` on `aws` fills the homelab first, and AWS takes the overflow. Lower priority numbers run first.

### 2. Describe the project (once per repository)

```sh
cd ~/code/my-app
spotexec init               # writes .spotexec.toml: lockfiles, package manager, common tasks
spotexec container init     # optional: a Dockerfile with this project's toolchain
spotexec doctor             # checks config, SSH, the helper, AWS credentials, AMI, VPC, quota
```

### 3. Run things

```sh
sx pnpm test                     # sync → run remotely → write back changes
sx pnpm prettier --write .       # formatted files land back in your worktree
sx test --reporter=dot           # a named task from .spotexec.toml, with extra args
sx verify                        # a parallel task: typecheck and test fan out across slots
spotexec status                  # what's running where, and idle timers
```

The first AWS run boots a VM, which takes about a minute plus your bootstrap. Later runs reuse it until it has been idle for `idle_timeout`. [Bake an image](#baked-images) to make cold boots close to bare boot time.

## AWS setup

This is the full path from an AWS account to a working Spot fleet. It takes about ten minutes, plus however long a quota increase takes if your account needs one.

1. [Install the AWS CLI](#1-install-the-aws-cli)
2. [Sign in](#2-sign-in)
3. [Grant permissions](#3-grant-permissions)
4. [Choose a region](#4-choose-a-region)
5. [Check your Spot vCPU quota](#5-check-your-spot-vcpu-quota)
6. [Check the network](#6-check-the-network)
7. [Configure the runner](#7-configure-the-runner)
8. [Verify end to end](#8-verify-end-to-end)

### 1. Install the AWS CLI

spotexec signs its own AWS requests, so plain access keys work without the CLI. Install AWS CLI v2 anyway if you sign in with IAM Identity Center (SSO), `aws login` or an assume-role profile. The CLI resolves those, and you will want it for the quota and policy commands below.

```sh
brew install awscli        # macOS; other platforms: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
aws --version              # aws-cli/2.9 or later (2.32+ for `aws login`)
```

### 2. Sign in

Choose **one** of these. Name the profile whatever you like; the examples use `spotexec`.

**A. IAM Identity Center (SSO).** Recommended for organizations.

```sh
aws configure sso --profile spotexec   # start URL, SSO region, account, permission set
aws sso login --profile spotexec       # again whenever the session expires
```

**B. `aws login`.** This uses your AWS Management Console sign-in as an IAM user or role (not IAM Identity Center). It needs AWS CLI 2.32 or later. IAM users and roles also need the AWS managed policy `SignInLocalDevelopmentAccess` alongside the spotexec policy; see the [`aws login` reference](https://docs.aws.amazon.com/cli/latest/reference/login/).

```sh
aws login --profile spotexec
```

**C. IAM user access keys.** The simplest option for a personal account. Use a dedicated user with only the [policy below](#3-grant-permissions), not your root user. Create the user and key from an admin session (or in the IAM console):

```sh
aws iam create-user --user-name spotexec
aws iam create-access-key --user-name spotexec      # note the key ID and secret
aws configure --profile spotexec                    # paste them in
```

**D. Assume role.** Add a `role_arn` / `source_profile` (or `credential_process`) profile to `~/.aws/config`. The AWS CLI resolves it for spotexec.

Then point the runner at the profile with `profile = "spotexec"`. Without `profile`, spotexec uses `AWS_PROFILE`, then the `default` profile.

Credentials resolve in this order:
1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`, only when the runner sets no `profile`.
2. `aws configure export-credentials --profile <profile>` (AWS CLI v2), which covers every method above. When the CLI is installed its answer is final: an expired session is reported (`run aws sso login`), never silently replaced by other keys.
3. Without the AWS CLI, `~/.aws` is read directly:
   - static keys;
   - `credential_process`;
   - IAM Identity Center profiles, using and refreshing the token cached by `aws sso login`;
   - unexpired `aws login` sessions.

   Assume-role profiles, and refreshing an `aws login` session, need the AWS CLI.

### 3. Grant permissions

Attach this policy to the identity you signed in with. Save it as `spotexec-policy.json`. The first three statements are required.

The other five are optional:
- S3, for the [dependency cache](#dependency-cache) (without it, set `cache_bucket = "off"` on the runner);
- the two ECR statements, for Dockerfile-built [containers](#project-containers);
- `servicequotas:GetServiceQuota`, for `doctor`'s quota check;
- the last statement, for [image baking](#baked-images).

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": [
        "ec2:RunInstances", "ec2:CreateTags", "ec2:DescribeInstances", "ec2:TerminateInstances",
        "ec2:DescribeImages", "ec2:DescribeVpcs", "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups", "ec2:CreateSecurityGroup", "ec2:AuthorizeSecurityGroupIngress"
      ], "Resource": "*" },
    { "Effect": "Allow", "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:*::parameter/aws/service/canonical/*" },
    { "Effect": "Allow", "Action": "iam:CreateServiceLinkedRole", "Resource": "*",
      "Condition": { "StringEquals": { "iam:AWSServiceName": "spot.amazonaws.com" } } },
    { "Effect": "Allow", "Action": [
        "s3:CreateBucket", "s3:PutLifecycleConfiguration", "s3:ListBucket",
        "s3:GetObject", "s3:PutObject"
      ], "Resource": ["arn:aws:s3:::spotexec-cache-*", "arn:aws:s3:::spotexec-cache-*/*"] },
    { "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ecr:DescribeRepositories", "ecr:CreateRepository", "ecr:TagResource", "ecr:PutLifecyclePolicy",
        "ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
        "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"
      ], "Resource": "arn:aws:ecr:*:*:repository/spotexec/*" },
    { "Effect": "Allow", "Action": "servicequotas:GetServiceQuota", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ec2:StopInstances", "ec2:CreateImage", "ec2:DeregisterImage", "ec2:DeleteSnapshot"
      ], "Resource": "*" }
  ]
}
```

For an IAM user (option C), run this with an admin identity:

```sh
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
aws iam create-policy --policy-name spotexec --policy-document file://spotexec-policy.json
aws iam attach-user-policy --user-name spotexec --policy-arn "arn:aws:iam::$ACCOUNT:policy/spotexec"
```

For IAM Identity Center, add the JSON as an inline policy on the permission set you sign in with. For a role, attach it to the role.

**The Spot service-linked role.** The account needs the service-linked role `AWSServiceRoleForEC2Spot` before its first Spot request. The `iam:CreateServiceLinkedRole` statement lets that request create it. If your identity can't create roles, have an admin run this once (an "already exists" error is fine):

```sh
aws iam create-service-linked-role --aws-service-name spot.amazonaws.com
```

### 4. Choose a region

A runner's region comes from, in order:
1. its `region` key;
2. `AWS_REGION`;
3. `AWS_DEFAULT_REGION`;
4. `us-east-1`.

The `region` in your `~/.aws/config` profile is **not** used, so set `region` on the runner explicitly.

Pick the region that is:
- **Closest to you.** Every run makes several SSH round trips (handshake, sync, streaming output, write-back). A nearby region keeps small commands under a second of overhead.
- **Stocked with your instance types.** These lookups aren't in the spotexec policy, so run them from an admin session. For example, check that the region offers `c7a.xlarge`:
  ```sh
  aws ec2 describe-instance-type-offerings --region us-east-2 \
    --filters Name=instance-type,Values=c7a.large,c7a.xlarge --query 'InstanceTypeOfferings[].InstanceType'
  ```
- **Cheap and roomy for Spot.** Compare current prices, and ask AWS how likely a request is to be filled (scores 1–10):
  ```sh
  aws ec2 describe-spot-price-history --region us-east-2 --instance-types c7a.xlarge \
    --product-descriptions Linux/UNIX --start-time "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --query 'SpotPriceHistory[].[AvailabilityZone,SpotPrice]' --output table
  aws ec2 get-spot-placement-scores --region us-east-2 --instance-types c7a.xlarge c6a.xlarge \
    --target-capacity 4 --region-names us-east-1 us-east-2 us-west-2
  ```

Everything spotexec creates is per region:
- the Spot quota;
- the S3 dependency cache;
- baked AMIs;
- the security group.

Moving a runner to another region therefore starts with a cold cache, and you'll need to run `image bake` again.

The EC2 console is regional too. If you don't see your instances, switch the console's region selector; `spotexec doctor` prints the region and tag to look for.

To span regions, define one runner per region and order them with `priority`, so the second only takes overflow.

### 5. Check your Spot vCPU quota

Spot capacity is limited by a per-region vCPU quota, *All Standard (A, C, D, H, I, M, R, T, Z) Spot Instance Requests* (`L-34B43A08`). New accounts often start very low, sometimes 5 vCPUs, which is not enough for even two 4-vCPU VMs.

You need at least `max_instances × vCPUs per instance`, summed over every fleet in the region. For example, 4 × `c7a.xlarge` needs 16 vCPUs. `spotexec doctor` shows the current value and what the runner may use. To check or raise it:

```sh
aws service-quotas get-service-quota --profile spotexec --region us-east-2 \
  --service-code ec2 --quota-code L-34B43A08 --query Quota.Value
# Requesting an increase needs an admin session (it isn't in the spotexec policy):
aws service-quotas request-service-quota-increase --region us-east-2 \
  --service-code ec2 --quota-code L-34B43A08 --desired-value 64
```

Small increases are usually approved within minutes to hours. Two features use on-demand instances, which count against the separate on-demand quota (`L-1216C47A`):
- `on_demand_fallback`;
- the `image bake` builder.

### 6. Check the network

By default nothing needs setting up. spotexec launches into the region's **default VPC** and:
- requests a public IPv4 address per instance (AWS bills public IPv4 addresses by the hour);
- creates a `spotexec-ssh` security group allowing TCP 22 from `ssh_cidr`. The default is `0.0.0.0/0`, with key-only authentication; narrow it to your address when it's stable, for example `ssh_cidr = "203.0.113.4/32"`.

Your machine must be able to make outbound SSH connections (TCP 22) to EC2 public addresses. Some corporate networks block this.

If the region has no default VPC (deleted, or blocked by your organization), set `subnet_id` to a **public** subnet: one whose route table sends `0.0.0.0/0` to an internet gateway. spotexec then creates its security group in that subnet's VPC, or uses `security_group_ids` if you set them. Those groups must allow inbound TCP 22 from your address, and outbound HTTP/HTTPS for packages, the S3 cache and ECR.

### 7. Configure the runner

In `~/.config/spotexec/config.toml`, here are two fleets: small VMs for light jobs, with bigger ones taking the overflow:

```toml
version = 1
default_pool = ["aws2", "aws4"]

[runners.aws2]
type = "aws-spot"
profile = "spotexec"
region = "us-east-2"
instance_types = ["c7a.large", "c6a.large", "m7a.large"]      # 2 vCPUs
max_slots = 1
max_instances = 4
priority = 10
idle_timeout = "10m"
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300 && apt-get install -y build-essential
"""
user_bootstrap_script = """
curl -fsSL https://mise.run | sh
~/.local/bin/mise use -g node@22 pnpm@9
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'

[runners.aws4]
type = "aws-spot"
profile = "spotexec"
region = "us-east-2"
instance_types = ["c7a.xlarge", "c6a.xlarge", "m7a.xlarge"]   # 4 vCPUs
max_slots = 2
max_instances = 4
priority = 20
idle_timeout = "10m"
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300 && apt-get install -y build-essential
"""
user_bootstrap_script = """
curl -fsSL https://mise.run | sh
~/.local/bin/mise use -g node@22 pnpm@9
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
```

The VMs run **Ubuntu 24.04** (login user `ubuntu`), resolved fresh from Canonical's public SSM parameter for the runner's architecture. `bootstrap_script` runs as root under `bash -e`, so a failing step fails the bootstrap (see [`BOOTSTRAP_FAILED`](#operations-and-troubleshooting)). Use it to install what your commands need, and keep both scripts idempotent.

### 8. Verify end to end

```sh
spotexec doctor aws2           # credentials, identity, AMI, network, quota, console region
spotexec up aws2               # boots one VM and waits until its bootstrap has finished
spotexec status                # the VM, its slots and its idle timer
cd ~/code/my-app && spotexec init   # spotexec only syncs Git repos or directories with .spotexec.toml
sx -r aws2 uname -a            # a first remote command
spotexec image bake aws2       # optional: snapshot the bootstrap for ~bare-boot cold starts
spotexec down aws2             # or simply let it idle out
```

If `up` fails, the error says why: a credential problem names the login command to run, a capacity error names the instance type, and a bootstrap failure prints the end of the bootstrap log.

### What gets created in your account

| Resource | When | Cleanup |
| --- | --- | --- |
| Spot instances tagged `spotexec-runner` / `spotexec-owner` | On demand | They terminate themselves; `spotexec down` terminates them now. |
| Security group `spotexec-ssh` (`spotexec-ssh-<hash>` for a custom `ssh_cidr`, one per VPC) | First launch without `security_group_ids` | Delete by hand if you stop using spotexec. |
| S3 bucket `spotexec-cache-<account>-<region>` | First run with `[deps]` (unless `cache_bucket = "off"`) | Objects expire after 14 days. |
| ECR repositories `spotexec/<repo-id>` | First Dockerfile build | Keeps the newest 10 images. |
| AMIs and snapshots | `spotexec image bake` | `spotexec image clear`. |

The SSH key is generated locally at `~/.config/spotexec/keys/id_ed25519` and injected through user-data, so no EC2 key pair is involved. Each instance gets its own `known_hosts` file, filled on first connect.

### Cost guardrails

The fleet shuts itself down without your laptop. One case is not covered: a VM whose kernel hangs before its timers fire. For a backstop that is fully independent of spotexec, add either of these to your account:
- an **AWS Budgets** alert;
- an **EventBridge** rule that terminates `spotexec-runner`-tagged instances older than N hours.

`spotexec status` lists pending and running instances only. Stopped instances (whose volumes still bill) don't appear there; `spotexec down` terminates them along with the rest.

## Running commands

```sh
sx [flags] <command> [args...]        # run a command remotely
sx [flags] -- <command> [args...]     # same, when the command is named like a subcommand
sx <task> [args...]                   # named task from .spotexec.toml
spotexec exec -- <command>            # reuse the last slot, without syncing
```

`sx` stops reading its own flags at the first word that isn't one, so `sx pnpm vitest run --watch=false` passes everything after `pnpm` through unchanged. Several arguments are shell-quoted one by one. A single argument runs as a shell line, as in `sx 'pnpm lint && pnpm test'`. Absolute paths under your worktree are mapped to the remote workspace.

The synced project is the enclosing Git worktree, or the nearest directory with a `.spotexec.toml`. Anywhere else, spotexec refuses to sync: it won't upload, say, your home directory with `~/.aws` and `~/.ssh` in it. Run `spotexec init` to mark a non-Git directory as a project, or use `--sync none`.

### Run flags

| Flag | Meaning |
| --- | --- |
| `--timeout D` | Wall-clock limit (default `5m`, or the project `timeout`). |
| `--queue-timeout D` | How long to wait for a free slot (default `10m`). |
| `--sync auto\|in\|none` | `auto` syncs and writes back; `in` syncs and discards changes; `none` runs on whatever the slot has. |
| `--writeback always\|on-success\|never` | When changes come back. |
| `-r, --runner NAME` / `--pool A,B` | Restrict where the command may run. |
| `--exclusive` | Take every slot on a machine (the job gets all its CPUs). |
| `--priority N` | Queue priority when every slot is busy (higher first). |
| `--no-boot` | Never launch VMs for this command; use only what is running. |
| `--pull GLOB` | Also copy these gitignored paths back (repeatable). |
| `--no-deps` | Skip dependency hydration. |
| `--no-retry` | Don't replay the run if its VM is lost. |
| `--fallback-local` | Run locally if no runner could be reached (never after the command started remotely). |
| `--force-large-sync` | Allow syncing more than 100k files or 5 GiB. |
| `--json` / `--pretty` / `-q, --quiet` | Event format: NDJSON, spinner, or nothing. |
| `--events-file PATH` | Also append NDJSON events to a file. |
| `-C, --dir DIR` | Run as if started in `DIR`. |

### Environment on the runner

Every command gets:

| Variable | Value |
| --- | --- |
| `SPOTEXEC=1`, `CI=true` | Marks a remote run. |
| `SPOTEXEC_RUNNER` | Runner name. |
| `SPOTEXEC_WORKSPACE` | Absolute path of the remote checkout. |
| `SPOTEXEC_SLOT` | Host-wide slot number, unique across repos. |
| `SPOTEXEC_PORT_BASE` | `20000 + slot × 1000`. Bind test servers here and parallel jobs never collide. |
| `SPOTEXEC_CPUS` | This job's share: vCPUs ÷ slots, or all of them with `--exclusive`. |
| `SPOTEXEC_SLOTS` | Slots per machine. |
| `SPOTEXEC_RUN_ID`, `SPOTEXEC_ATTEMPT` | Stable across a replay; `SPOTEXEC_ATTEMPT` is `2` on the replay. |
| `COMPOSE_PROJECT_NAME` | `sx_<repo>_sNN`, so Docker Compose stacks don't collide. |
| `FORCE_COLOR`, `CLICOLOR_FORCE` | Set when your stdout is a terminal. |

Plus anything in `env_allow` (copied from your environment) and `env` (fixed values). Outside a project container, before each command the runner sources the login profile (`/etc/profile`, then `~/.bash_profile`, `~/.bash_login` or `~/.profile`) and then `remote_path_setup`.

### Exit codes

| Code | Meaning |
| --- | --- |
| *command's own* | The remote command's exit status, passed through. |
| `124` | The command hit `--timeout`. |
| `125` | spotexec itself failed: no runner, SSH, sync, bootstrap. In a parallel task, a `125` outranks a test failure, so agents can tell "tests failed" from "couldn't run tests". |
| `128+N` | The command died from signal N. |
| `130` | Interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`). |
| `1`, `2` | Usage or configuration error. |

## Using spotexec with coding agents

### Machine-readable output

When stderr isn't a terminal (or with `--json`, or `SPOTEXEC_AGENT=1`), lifecycle events go to stderr as NDJSON. The command's own stdout and stderr pass through untouched:

```json
{"event":"slot_acquired","runner":"aws","slot":3,"connect_ms":13,"waited_ms":0}
{"event":"sync_in","mode":"fingerprint_hit","files":0,"bytes":0,"duration_ms":0}
{"event":"sync_out","mutated_files":["src/index.ts"],"conflicts":[],"duration_ms":31}
{"event":"result","exit_code":0,"duration_ms":4120,"timed_out":false}
```

Other events include:
- `queued`, `queue_position`, `placed`;
- `scale_out`, `provision`, `instance_running`, `instance_ready`;
- `manifest`, `deps`, `image`, `exec`, `pull`, `cancel`;
- `spot_interruption`, `spot_rebalance`, `retry`;
- `warning` events with stable codes (`SYNC_CONFLICT`, `RUNNER_LOST`, `SPOT_INTERRUPTION`, `TIMEOUT`, `LOCAL_FALLBACK`, …);
- `error` with code `SPOTEXEC_ERROR` when spotexec itself fails (exit `125`). For example, a `BOOTSTRAP_FAILED` message.

### Claude Code hook

```sh
spotexec hook install --claude    # .claude/settings.local.json (--shared: settings.json, --global: ~/.claude)
spotexec hook check 'pnpm test'   # show what the hook would do with a command line
```

The hook rewrites heavy Bash commands to `spotexec run -- '<cmd>'` before they execute:
- **Offloaded:** tests, `tsc`, linters, formatters, builds, Maven and Gradle verify goals.
- **Kept local:** `git`, `ls`, `rg`, dev servers, watch modes, pipes, redirects, background jobs, anything that `cd`s out of the repo, and anything prefixed with `SPOTEXEC_LOCAL=1`. Use that prefix, for example, for a test that needs `localhost:5432`.
- **Scope:** it acts only in projects with a `.spotexec.toml`, and approves nothing itself. Your permission rules see the rewritten `spotexec run -- <cmd>`, so allow `Bash(spotexec run:*)` if you pre-approve test commands.

The `[hook]` table tunes it:
- `when_load_above = 0.6` offloads only when the laptop is busy;
- `patterns` / `exclude` add your own regexes;
- `mode = "block"` denies the command with instructions, instead of rewriting it;
- `disabled = true` turns it off.

`spotexec hook install --codex` appends an `AGENTS.md` section for Codex and other harnesses without hooks. `--uninstall` removes either one.

## How it works

```
laptop                                         runner (SSH box or Spot VM)
──────                                         ───────────────────────────
sx ─┬─ manifest: git ls-files + blob IDs       spotexec-helper (uploaded on first use)
    ├─ OpenSSH ControlMaster ──────── SSH ───▶  ├─ flock'd concurrency tokens + workspace slots
    └─ framed RPC over stdio ─────────────────▶ ├─ applies the sync, hydrates deps
                                                ├─ runs the command in its own process group
EC2 API (your credentials) ◀── sx               ├─ snapshots files before/after → mutation list
  RunInstances / Describe / Terminate           └─ dependency cache (reflink/copy, S3 restore)
  tags: spotexec-runner, spotexec-owner
                                                cloud-init idle watchdog (systemd timer) ─▶ poweroff
```

A single `sx` invocation:

1. **Builds a manifest** of the worktree: every file Git tracks or would track (respecting `.gitignore`, `.spotexecignore` and `sync.exclude`), with Git blob IDs. Clean tracked files reuse the index IDs and are never read.
2. **Acquires a slot.** It tries runners in pool order and takes a free slot without blocking. When every slot is busy it joins a fair local queue and, for Spot fleets, registers demand that may launch more VMs.
3. **Syncs.** The helper reports which blobs the slot is missing, and only those travel, as one gzip'd tar over the same SSH channel. An unchanged worktree is a fingerprint hit and sends nothing.
4. **Hydrates dependencies** from a cache keyed by your lockfiles, or runs your install command once and caches the result.
5. **Runs the command** in its own process group, with a wall-clock timeout and heartbeats in both directions. Output streams back live.
6. **Writes back.** The helper hashes the workspace after the run and reports changed, created and deleted files. The CLI applies each one only if your local copy still matches what was synced.

Managing Spot instances needs no service running anywhere. Each concern below is covered by your laptop, the VM itself, or EC2:

| Concern | Who handles it |
| --- | --- |
| Launching a VM | The CLI, on demand, calling EC2 with your own AWS credentials. |
| Knowing which VMs exist | EC2 itself. Instances carry `spotexec-runner` and `spotexec-owner` tags; a local record only caches addresses. |
| Tearing VMs down | **The VM itself.** cloud-init first installs a systemd timer that powers the VM off after `idle_timeout` without activity, and unconditionally after `max_lifetime`. Instances launch with `InstanceInitiatedShutdownBehavior=terminate`, so power-off means terminate and billing stops. Closing your laptop lid doesn't matter. |
| Not shutting down mid-job | While it owns a slot, the helper holds a shared `flock` on `/var/tmp/spotexec/lifecycle.lock`. The watchdog needs that lock exclusively before it shuts down. |
| Stuck sessions (laptop asleep, network blackhole) | The CLI and helper exchange heartbeats. After 45s of silence the helper stops the command and releases the slot, so the idle clock resumes. |
| Spot interruptions | The helper polls IMDSv2 for the 2-minute notice and marks the VM draining. The CLI starts finding replacement capacity right away and replays the run if the VM dies first. |
| Concurrent agents booting the same fleet | A local `flock`-protected launch ledger, plus an idempotent `ClientToken` on `RunInstances`. |
| A VM that never finishes booting | Reaching `running` and each bootstrap script are bounded by `boot_timeout`. A VM still not ready after `2 × boot_timeout + 2m` is terminated by ID by the next `sx` or `spotexec daemon` that sees it; otherwise its own watchdog powers it off. |

## Scaling, queueing and CPU sizing

Every `sx` process schedules for itself, against shared local state:

- **Placement.** Static (`ssh`) runners are tried first, by priority. On a fleet, a job takes a free slot on the **oldest** instance that has one. Packing work onto old VMs lets the newest ones go idle and scale in, with no coordinator.
- **Queueing.** When every slot is busy, the job takes a ticket in a fair local queue: priority first, then arrival. Spinners show `queued #3 of 9`, and agents get `queue_position` events.
- **Scaling out.** A queued job registers demand, and the fleet launches while waiting demand exceeds the capacity already booting, up to `max_instances`. Lower-priority fleets only launch for demand the higher-priority ones can't cover. A failed launch backs the fleet off for 20s.
- **Scaling in.** Each VM powers itself off after `idle_timeout`. Nothing has to tell it to.
- **CPU sizing.** Point your test runner at `$SPOTEXEC_CPUS` and it uses its fair share of the VM:

  ```sh
  vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS   # Vitest 2 needs --minWorkers=1 on small shares
  jest --maxWorkers=$SPOTEXEC_CPUS
  pytest -n $SPOTEXEC_CPUS
  go test -p $SPOTEXEC_CPUS ./...
  ```

To mix machine sizes, define two fleets: a 2-vCPU fleet for light jobs and a 4-vCPU fleet for heavy ones. Route tasks to them with `pool`, or let overflow spill from one to the other by priority. [docs/POOL.md](docs/POOL.md) has the full scheduling design.

## The life of a Spot VM

1. **Launch.** A one-time Spot request is made at a price capped by `max_price` (default: the on-demand price), with IMDSv2 required and a public IPv4 address. Capacity errors move on to the next instance type; with `on_demand_fallback`, the last resort is on-demand.
2. **Arm.** The user-data script's first action installs the idle watchdog (a systemd timer) plus an independent `max_lifetime` backstop. Only then does it authorize the SSH key and run your bootstrap scripts. `spotexec cloud-init <runner>` prints the exact script.
3. **Bootstrap.** Your scripts run under the lifecycle lock, so a long setup can't be mistaken for idleness. If one fails, the VM is marked failed: it refuses work and returns `BOOTSTRAP_FAILED` with the tail of `/var/log/spotexec-bootstrap.log`. The fleet then stops launching for an hour instead of burning money on a broken script.
4. **Serve.** Jobs acquire slots, and every session refreshes the activity clock.
5. **Interruption.** On a Spot interruption notice the VM drains: running jobs continue and new ones are refused. The CLI starts finding replacement capacity right away. A job that finishes within the 2 minutes is used normally. A job whose VM disappears first is **replayed once** on the replacement: sync, deps, run and write-back. Nothing from the lost attempt was written locally. Rebalance recommendations are only reported, because EC2 often sends them the moment a VM boots, and draining on them makes a fleet churn.
6. **Exit.** After `idle_timeout` with no sessions, or immediately once a draining VM has no sessions, or at `max_lifetime`, the VM powers itself off, which terminates it.

> **Replays and side effects.** A replayed command may already have run to completion the first time. Commands with outside effects (deploys, publishing) should dedupe on `SPOTEXEC_RUN_ID`, or run with `--no-retry`.

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

## Dependency cache

With `[deps]` configured, each slot's `cache_dirs` are restored from a cache keyed by:
- the lockfile contents and the install command;
- the cache dirs;
- the runner's OS and architecture;
- in a [project container](#project-containers), the image's immutable ID;
- otherwise, the runner's configured AMI, bootstrap scripts and `remote_path_setup`.

With a container, native modules never cross toolchain versions. Without one, pin tool versions in your bootstrap: upgrading Node on an `ssh` host, or a newer Ubuntu image behind `ami = "auto"`, doesn't change the key.

- **On a runner,** caches are hydrated into the slot with `reflink` where the filesystem supports it (XFS, Btrfs, APFS), else `copy`. `hardlink` is opt-in, because shared inodes let one slot corrupt another's `node_modules`.
- **Across Spot VMs,** the first VM to install a dependency set uploads it as one compressed archive to a private S3 bucket in your account, `spotexec-cache-<account>-<region>`. The bucket is created on the first `[deps]` run, with S3's default Block Public Access and a 14-day expiry. Without the S3 permissions, set `cache_bucket = "off"`. Later VMs download it instead of installing.
- **VMs never hold AWS credentials.** The CLI hands each one presigned URLs, and only for a bucket your account owns.
- **Synced files are never touched.** A tracked file that lives inside a cache dir is left alone by hydration and cache clearing.

## Baked images

```sh
spotexec image bake aws      # build an AMI from the runner's bootstrap
spotexec image list aws
spotexec image clear aws
```

`image bake` boots an on-demand builder and runs `bootstrap_script`. It installs Docker and buildx, pre-pulls the project's public container image (`--pull IMAGE` adds more), and runs `user_bootstrap_script`. Then it sanitizes the instance (cloud-init state, SSH host keys, logs, the watchdog) and snapshots it.

From then on the fleet boots from the newest bake whose bootstrap hash matches, and skips the bootstrap, so a new VM is ready in roughly bare boot time. The per-instance setup (watchdog, SSH key, timers) still runs on every VM.

Changing either bootstrap script, `arch` or `user` makes old bakes stop matching; run `image bake` again. Only the two newest bakes per runner are kept, and older images and their snapshots are deleted.

## Project containers

`[container]` runs every command, and the dependency install, inside a Docker image on the runner. The image carries the project's toolchain, so the runner itself only needs Docker; on AWS, spotexec installs Docker for you.

### Scaffold, edit, build

```sh
spotexec container init              # writes .spotexec/Dockerfile and adds [container] to .spotexec.toml (creating it if needed)
$EDITOR .spotexec/Dockerfile         # add the system packages and tools your commands need
spotexec container build -r aws      # build it on a runner now; on AWS, push it to ECR
sx pnpm test                         # every command now runs in the image
```

`container init` detects the stack and writes a commented Dockerfile with a pinned base image:

| Stack | Base image |
| --- | --- |
| Node.js (npm, pnpm, Yarn) | `node:<major>-bookworm`. The major version comes from `.nvmrc`, `.node-version` or `engines.node`; pnpm is pinned from `packageManager`. |
| Bun | `oven/bun:1` |
| Python with uv | `ghcr.io/astral-sh/uv:python<x.y>-bookworm`, with the version from `.python-version` |
| Python | `python:<x.y>-bookworm` |
| Go | `golang:<x.y>-bookworm`, with the version from `go.mod` |
| Rust | `rust:<version>-bookworm`, with a numeric version from `rust-toolchain(.toml)` (otherwise `1`), plus clippy and rustfmt |
| Maven / Gradle | `eclipse-temurin:21-jdk`, plus `maven` when there's no `./mvnw` (Gradle projects use `./gradlew`) |
| Anything else | `ubuntu:24.04` with build-essential |

`init` also stages the Dockerfile with `git add`. Untracked files under `.spotexec/` aren't synced, and the runner builds from the synced copy. Commit the file with the project; once Git tracks it, your edits sync like any other file. `--path` writes it elsewhere, and `--force` overwrites an existing one.

`container build` syncs the worktree, then builds the image on the chosen runner, or pulls it for `image =`. On AWS it pushes the result to a private ECR repository, `spotexec/<repo-id>`, which keeps the newest 10 images, and waits for the push to finish. It skips the dependency install, and it reports build and push errors straight away. Without `container build`, the first `sx` command builds the image the same way, but pushes it in the background, holding the VM up until the push is done. Every later VM pulls the image from ECR instead of rebuilding it.

### Writing the Dockerfile

- **Don't copy your source in.** The worktree is mounted at the same path at run time, and dependencies (`node_modules`, `.venv`, …) come from the [dependency cache](#dependency-cache). Install tools, not code.
- **Commands run as the runner's non-root user,** with `HOME=/home/spotexec`, a per-slot directory mounted at run time that persists between runs. Install toolchains system-wide (for example under `/usr/local`), never under `/root` or `/home/spotexec`: anything built into `/home/spotexec` is hidden by the mount. Point tool caches at `$HOME` so they survive between runs; the Go and Rust scaffolds already do this.
- **Use the image's own `ENV`.** Inside the container, the runner's login profile and `remote_path_setup` don't apply. The image needs `/bin/sh`, and bash is used when present.
- **Rebuilds are content-addressed.** The image is tagged by the content of the Dockerfile and its build context (default: the Dockerfile's directory). It is rebuilt only when those change. `.dockerignore` applies exactly as in `docker build`: `<Dockerfile>.dockerignore` wins over `<context>/.dockerignore`, and ignored files are neither sent nor hashed. With `context = "."` and an ignore file that keeps only manifests, editing source never triggers a rebuild.

### Settings

```toml
[container]
dockerfile = ".spotexec/Dockerfile"   # built on the runner; or:
# image = "node:22-bookworm"          # pulled as-is
# context = "."                       # default: the Dockerfile's directory
# options = ["--shm-size=1g"]         # extra `docker run` arguments
# user = "root"                       # "runner" (default: files keep the right owner) or "root"
# timeout = "20m"                     # pull/build limit
```

- **Paths and ports.** Write-back, path rewriting and snapshots work unchanged. Containers use host networking, so `SPOTEXEC_PORT_BASE` still applies.
- **Control.** Docker always runs as the runner user, so Ctrl-C and timeouts can always stop a container. `container.options` may not set `--name`, `--rm`, `--detach`, `--entrypoint`, `--user` or `--workdir`, because spotexec relies on those to find, stop and clean up containers.
- **Caching.** The dependency cache is keyed by the image's immutable ID, so a moving tag like `node:22` never restores modules built for an older image.

## Write-back and artifacts

The manifest records the Git blob ID of every synced file: the **baseline**. After the command, the helper reports what changed by content, not timestamps, and each path is decided against your worktree *now*:

| Your local file now | Outcome |
| --- | --- |
| Equals the remote result | Nothing to do. |
| Equals the baseline | The remote version is applied atomically (temp file, then rename). |
| Anything else | You edited it during the run. **Your file is kept**, the remote version is saved to `<git-dir>/spotexec/conflicts/<run-id>/<path>`, and a `SYNC_CONFLICT` warning is emitted. |

Deletions apply deepest-first, then writes. New files only come back if Git wouldn't ignore them, so `dist/`, `target/` and `node_modules` never travel back. A runner can never write into `.git`, under any spelling.

To get **ignored outputs** back (coverage reports, build output), use one of:
- `--pull coverage/` on a run (globs are single-level: `--pull 'packages/*/coverage'`, and `**` is not recursive);
- `pull_artifacts` in `.spotexec.toml`;
- `spotexec pull <glob>` afterwards, from the last-used slot (`--dest DIR` to land them elsewhere).

Pulled files never overwrite tracked or non-ignored files, and never land in another repository or a git directory.

## Configuration

There are two files, and the split is deliberate.

| File | Holds | Committed? |
| --- | --- | --- |
| `~/.config/spotexec/config.toml` | Your hardware, accounts, credentials, network | Never |
| `.spotexec.toml` at the repository root | How this project syncs, installs and runs | Yes |

Settings resolve in this order, highest first:
1. CLI flags.
2. `SPOTEXEC_*` environment variables: `SPOTEXEC_TIMEOUT`, `SPOTEXEC_SYNC`, `SPOTEXEC_RUNNER`, `SPOTEXEC_POOL`, `SPOTEXEC_PRIORITY`, `SPOTEXEC_JSON=1`, `SPOTEXEC_AGENT=1`.
3. `.spotexec.toml`.
4. The global config.
5. Built-in defaults.

Three exceptions to that order:
- a runner (`-r` or `SPOTEXEC_RUNNER`) beats any pool;
- `--json` / `SPOTEXEC_JSON=1` beats `--pretty`;
- a task's own `pool` beats `SPOTEXEC_POOL`.

### Global config: `~/.config/spotexec/config.toml`

```toml
version = 1
default_pool = ["homelab", "aws"]   # runners tried for every command, by priority
# local_fallback = false            # run locally when no runner is reachable (see --fallback-local)

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]
max_slots = 4
priority = 10

[runners.aws]
type = "aws-spot"
region = "us-east-2"
profile = "dev"
instance_types = ["c7a.4xlarge", "c6a.4xlarge", "m7a.4xlarge"]
max_slots = 8
max_instances = 4
min_instances = 0                 # kept warm only while `spotexec daemon` runs
priority = 20
idle_timeout = "10m"
max_lifetime = "12h"
volume_gb = 64
hourly_cost = 0.25                # used by `spotexec monitor` for burn rate
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300 && apt-get install -y build-essential
"""
user_bootstrap_script = """
curl -fsSL https://mise.run | sh
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
```

#### Keys for every runner

| Key | Default | Meaning |
| --- | --- | --- |
| `type` | (required) | `ssh` or `aws-spot` (`sim` is the local simulator; see [Development](#development)). |
| `max_slots` | vCPUs ÷ 4, at least 1 | Concurrent jobs per machine (1–44). |
| `priority` | `0` | Lower runs first. Later runners in the pool only take overflow. |
| `work_root` | `~/.cache/spotexec` | Where slots, caches and the helper live on the runner. |
| `remote_path_setup` | (none) | Shell run before each command, after the login profile (for example `PATH` for mise or nvm). Not used inside a [project container](#project-containers). |
| `ssh_options` | (none) | Extra `-o` options for `ssh`. |
| `hourly_cost` | (none) | Price per machine-hour of a fleet instance, for `spotexec monitor`. |

#### `ssh` runners

| Key | Meaning |
| --- | --- |
| `host` / `hosts` | Routes to the same machine: `user@host`, `host`, or a `~/.ssh/config` alias. They are dialed in parallel, and an earlier entry wins if it connects within 500ms of the first answer. |
| `user` | Login user when the host omits `user@`. |
| `identity_file` | SSH private key (default: your ssh config). Fleets always use the generated key. |

#### `aws-spot` runners

| Key | Default | Meaning |
| --- | --- | --- |
| `region` | `AWS_REGION`, `AWS_DEFAULT_REGION`, else `us-east-1` | EC2 region. |
| `profile` | (env / default) | AWS profile to use. |
| `instance_types` | `c7a.4xlarge`, `c6a.4xlarge`, `m7a.4xlarge` | Tried in order on capacity errors. All must share one architecture. |
| `arch` | from the first instance type | `amd64` or `arm64` (Graviton). |
| `max_instances` | `4` | Fleet ceiling. |
| `min_instances` | `0` | Warm floor, kept only while the [pool manager](#pool-manager-optional) runs. |
| `idle_timeout` | `10m` | The VM powers off after this long with no work. |
| `max_lifetime` | `12h` | Hard cap, regardless of activity. Must exceed `boot_timeout`. |
| `boot_timeout` | `15m` | Longest wait for first-boot setup; a VM that takes longer is terminated. |
| `bootstrap_script` | (none) | Runs as root on first boot, after the watchdog is armed. Bounded by `boot_timeout`. |
| `user_bootstrap_script` | (none) | Then runs as the login user, in a login shell. |
| `max_price` | the on-demand price | Spot bid cap in USD per hour, as a string (`"0.40"`). |
| `on_demand_fallback` | `false` | Launch on-demand when no Spot capacity is available. |
| `volume_gb` | `64` | Root gp3 volume (at least 8). |
| `ami` | `auto` | `auto` uses the newest matching bake, else Ubuntu 24.04. `base` ignores bakes. An `ami-…` ID pins one image. |
| `user` | `ubuntu` | Login user of the AMI. |
| `subnet_id`, `security_group_ids` | default VPC, a managed `spotexec-ssh` group | Network placement. |
| `ssh_cidr` | `0.0.0.0/0` | Source range for the managed group's SSH rule. Auth is key-only; narrow it if your IP is stable. |
| `cache_bucket` | `auto` | S3 [dependency cache](#dependency-cache): `auto`, a bucket name, or `off`. |

### Project config: `.spotexec.toml`

Every key is optional. `spotexec init` writes a starting point for npm, pnpm, yarn, bun, Go, Rust and Python projects.

```toml
version = 1
timeout = "5m"                 # wall-clock cap per command (exit 124 when exceeded)
grace_period = "5s"            # SIGTERM → SIGKILL delay on timeout or Ctrl-C
queue_timeout = "10m"          # how long to wait for a free slot
env_allow = ["NODE_OPTIONS"]   # local env vars forwarded (nothing else is)
env = { TZ = "UTC" }           # set on the runner for every command
pool = ["aws"]                 # overrides default_pool for this repo

[sync]
mode = "auto"                  # auto: write back · in: discard changes · none: don't sync
writeback = "always"           # always | on-success | never (eslint --fix exits 1 but still fixes)
exclude = [".env*", "*.pem"]   # on top of .gitignore and .spotexecignore
pull_artifacts = ["coverage/"] # gitignored outputs copied back after every run
# source = "git"               # git | directory (default: git when in a worktree)

[deps]
lockfiles = ["pnpm-lock.yaml"]
install = "pnpm install --frozen-lockfile"
cache_dirs = ["node_modules", "packages/*/node_modules"]
hydrate = "auto"               # auto (reflink, else copy) | reflink | copy | hardlink
# timeout = "20m"

[container]                    # optional; see "Project containers"
dockerfile = ".spotexec/Dockerfile"   # written by `spotexec container init`

[tasks.test]
run = "pnpm vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS {args}"

[tasks.typecheck]
run = "pnpm tsc --noEmit"

[tasks.verify]
parallel = ["typecheck", "test"]
fail_fast = false

[hook]
when_load_above = 0.6          # agents offload only when the laptop is busy
```

#### Tasks

| Key | Meaning |
| --- | --- |
| `run` | Shell command. `{args}` (as its own word, or `"{args}"`) becomes the arguments after the task name, passed as `"$@"`; without it they are appended. |
| `parallel` | Names of other (non-parallel) tasks to run at once, each in its own slot. Extra arguments are not forwarded. |
| `fail_fast` | Stop the remaining parallel tasks when one fails. |
| `timeout`, `sync`, `exclusive` | Per-task overrides of the run flags. |
| `pool`, `priority` | Send this task to a different fleet, or move it ahead in the queue. |

Task names can't shadow subcommands (`run`, `status`, `sync`…). `spotexec tasks` lists them.

### Project-defined fleets

A repository can declare its own fleet, built on one of your runners:

```toml
pool = ["ci-big"]

[runners.ci-big]
extends = "aws"                # one of YOUR runners in ~/.config/spotexec/config.toml
instance_types = ["c7a.2xlarge", "m7a.2xlarge"]
max_slots = 2                  # 4 vCPUs per job
max_instances = 3
```

A project runner **must** build on one of your global runners: name it with `extends`, or reuse its name to override it. It may only change sizing, scheduling and the VM's own setup: instance types, slots, instance counts, timeouts, price, volume, bootstrap scripts.

How you connect and which account and network are used stay in your global config. A project setting any of these is an error: `type`, `host`, `hosts`, `ssh_options`, `identity_file`, `work_root`, `profile`, `region`, `subnet_id`, `security_group_ids`, `ssh_cidr`, `user`, `cache_bucket`. A cloned repository therefore can't redirect your SSH, pick a different AWS account, or open a port. See [Security model](#security-model).

## Security model

The trust boundaries, from the inside out:

- **The VMs hold no AWS credentials.** They have no instance profile, and they get presigned URLs for exactly the objects they need. Instance metadata requires IMDSv2.
- **A runner can't write outside the worktree, or into `.git`, and can't read your other files.** Every path a runner sends back goes through these checks:
  - it must be clean and relative, with no `.git` segment under any case spelling;
  - it must not resolve through a symlink;
  - for a changed or deleted file, it must be in the manifest of what was synced;
  - for a new file, it must not be ignored, excluded, or already present locally, so a local file is never overwritten;
  - for a sync request, it must be in the manifest too, so a runner can't ask for `~/.aws/credentials`;
  - for a pull, it must match a pattern you asked for.
- **A cloned repository can't change how you connect.** `.spotexec.toml` can't set hosts, SSH options, keys, AWS profile, region, network or bucket. Project runners must extend one of your own runners.
- **Project config is still code for the runner.** Bootstrap scripts, tasks and `container.options` (for example `--privileged`) run with the runner user's rights on the runner. On a VM of your own, that only matters to you. On a **shared `ssh` runner**, treat `.spotexec.toml` like a CI config: review it before running untrusted repos.
- **Secrets stay out of process listings.** Forwarded environment values go to the runner in a `0600` file, not on the command line.
- **Slots are private.** The work root is `0700`. Lifecycle state files are writable only by the runner user's group.
- **The runner is trusted with your worktree.** Write-back is how formatters and codegen deliver their results, so a compromised runner can still change synced files and create new non-ignored ones, such as a `Makefile` or editor task config. Review `git status` after runs on machines you don't control.

## Operations and troubleshooting

```sh
spotexec doctor                 # config, SSH, ControlMaster, helper, credentials, AMI, VPC, quota
spotexec status [--json]        # runners, instances, slot holders, idle timers
spotexec monitor                # live load and burn-rate dashboard
spotexec up aws --count 2       # pre-warm VMs now (respects max_instances)
spotexec down aws | --all       # terminate fleet VMs now (and leftover bake builders)
spotexec unlock aws/i-0abc --slot 3  # kill a wedged run and free its slot (--all for every slot; name the instance when several run)
spotexec prune --older-than 7d  # remove stale dependency caches and slot workspaces on runners
spotexec sync-plan              # dry run: file count, bytes, largest files, exclusions
spotexec cloud-init aws         # print the exact first-boot script
```

| Symptom | Likely cause and fix |
| --- | --- |
| `BOOTSTRAP_FAILED` | Your `bootstrap_script` exited non-zero (or ran past `boot_timeout`). The error shows the log tail. Fix the script, then run `spotexec down <runner>`, which also clears the one-hour launch hold. |
| Jobs queue but no VM launches | Check `max_instances`, `spotexec daemon status` for a paused fleet, and the vCPU quota (`spotexec doctor`). A failed launch backs off for 20s. |
| `InsufficientInstanceCapacity` | Add more `instance_types`, or try another region or availability zone. |
| Sync is slow or refused as too large | Run `spotexec sync-plan`. Exclude build outputs and large fixtures with `sync.exclude` or `.spotexecignore`. |
| `SYNC_CONFLICT` | You edited a file during the run. Compare it with the saved copy under `.git/spotexec/conflicts/<run-id>/`. |
| A test needs a local service | Prefix it with `SPOTEXEC_LOCAL=1` (agent hook), or run it with plain `pnpm …` instead of `sx`. |
| Expired SSO session | `aws sso login` (or `aws login`). spotexec reports this instead of falling back to other keys. |

## Command reference

```
sx [flags] <command...>                      run remotely (shorthand for `spotexec run --`)
sx <task> [args...]                          named task; parallel tasks fan out across slots
spotexec run [flags] -- <cmd>                sync, run, write back
spotexec exec [flags] -- <cmd>               run in the last slot without syncing
spotexec tasks                               list tasks from .spotexec.toml
spotexec sync [flags]                        pre-sync a slot (and build the container image, install deps)
spotexec sync-plan [--json]                  dry-run the sync: files, bytes, largest files, exclusions
spotexec pull <glob>... [--dest DIR]         copy gitignored files from the last slot
spotexec status [runner...] [--json]         runners, instances, slot holders, idle timers
spotexec monitor [runner...] [--interval 2s] live load and burn-rate dashboard
spotexec up [runner] [--count N]             pre-warm fleet VMs (waits until ready)
spotexec down [runner...] [--all]            terminate fleet VMs (and leftover bake builders)
spotexec daemon start|stop|run               optional pool manager
spotexec daemon status [--json]              what the pool manager sees and has done
spotexec container init [--path P] [--force] scaffold a Dockerfile and wire it into .spotexec.toml
spotexec container build [run flags]         build the project image on a runner now (ECR on AWS)
spotexec image bake [runner] [--pull IMG]    bake an AMI from the runner's bootstrap
spotexec image list|clear [runner]           list or delete baked AMIs
spotexec doctor [runner...]                  verify config, SSH, helper, credentials, network, quota
spotexec unlock [runner[/id]] (--slot N | --all)
spotexec prune [runner[/id]...] [--older-than 7d] [--dry-run]
spotexec init [--global] [--force]           generate configs
spotexec hook install [--claude] [--codex] [--shared|--global] [--uninstall]
spotexec hook check '<command>'              show whether the hook offloads a command
spotexec cloud-init [runner]                 print the first-boot user-data
spotexec completion bash|zsh|fish|powershell shell completion script
spotexec version
```

## Files and state

| Path | Contents |
| --- | --- |
| `~/.config/spotexec/config.toml` | Global config (`SPOTEXEC_CONFIG`, `SPOTEXEC_CONFIG_DIR` or `XDG_CONFIG_HOME` override). |
| `~/.config/spotexec/keys/` | Generated SSH key for ephemeral runners. |
| `~/.config/spotexec/owner-id` | This machine's owner tag. Machines only ever see their own instances. |
| `~/.local/state/spotexec/` | Pool ledgers, queue tickets, instance records, `known_hosts`, daemon state and log (`SPOTEXEC_STATE_DIR` or `XDG_STATE_HOME` override). |
| `.spotexec.toml`, `.spotexecignore` | Project config and extra sync excludes. |
| `<git-dir>/spotexec/conflicts/` | Remote versions of files that conflicted on write-back (`.spotexec/conflicts/` outside Git). |
| `.spotexec/Dockerfile` | Project image from `spotexec container init`. |
| `~/.cache/spotexec/` | Local SSH ControlMaster sockets (`cm/`) and helper install markers. |
| Runner: `~/.cache/spotexec/` | Helper binary, slots, dependency caches (`work_root`). |
| Runner: `/var/tmp/spotexec/`, `/var/log/spotexec-bootstrap.log` | Lifecycle state and bootstrap log on ephemeral VMs. |

## Known limits

- **Process cleanup is not cgroup-based.** Commands run in their own process group. On Linux runners, processes that escaped it (for example via `setsid`) are killed after the run if their working directory is inside the slot. One that also left the workspace can survive, as can any escaped process on a macOS runner.
- **Write-back can't be atomic against editors that ignore locks.** Each file is checked before its new content is staged, and again just before the rename. A save landing in that last syscall window can still be replaced.
- **Case-only directory renames keep the old spelling locally.** On a case-insensitive filesystem (the macOS default), a remote `mv src Src` updates the files but leaves the directory named `src`. Case-only *file* renames are applied correctly.
- **Each machine gets its own instances.** Instances are tagged with this machine's owner ID, and a machine never discovers or terminates another's. To share VMs between two machines, copy `owner-id`. They then coordinate only through EC2's eventually consistent tag filters.
- **Scaling can't predict demand.** A VM launched for demand that clears before it boots sits idle until `idle_timeout`, then terminates itself.
- **Exclusive jobs across mismatched fleets.** Demand is counted per fleet, so an `--exclusive` job that could land on two fleets with different `max_slots` may briefly over-launch by one instance.
- **Shared SSO cache.** Without the AWS CLI, spotexec refreshes IAM Identity Center tokens itself. If an AWS CLI elsewhere (say, a dev container sharing `~/.aws`) refreshes the same token at the same moment, one side may need `aws sso login` again.
- **Providers are `ssh` and `aws-spot`.** The provider interface is ready for more.

## Development

Work happens on `develop` (GitFlow); releases are tagged on `main` and published automatically. See [CONTRIBUTING.md](CONTRIBUTING.md) for the branch model and release process.

```sh
make build          # helpers for all platforms, then bin/spotexec + bin/sx
make test           # unit tests (go test ./...; add -race locally)
make vet
make e2e            # builds, then runs all three end-to-end suites:
                    #   scripts/e2e.sh         SSH runner: sync, write-back, conflicts, tasks, deps, pull, exec, watchdog
                    #   scripts/e2e-pool.sh    Spot fleet scenarios on the Docker simulator
                    #   scripts/e2e-daemon.sh  pool manager scenarios on the simulator
```

The **`sim` provider** runs a Spot fleet as Docker containers on your machine, so the full lifecycle can be tested without an AWS account.
- Each instance is a container running sshd and booting the same cloud-init user-data.
- `spotexec sim interrupt` delivers an interruption notice; `spotexec sim rebalance` sends a rebalance recommendation.
- `spotexec sim build` builds the simulator image.
- Setting `SPOTEXEC_DAEMON_FAST=1` shrinks the pool manager's timings for tests.
- With a plain `go build` (no embedded helpers), `SPOTEXEC_HELPER_DIR` points at a directory of helper binaries.

The code is organized as:

| Package | Role |
| --- | --- |
| `cmd/spotexec`, `internal/cli` | Command surface. |
| `internal/client` | Run orchestration, pool planning, queueing, write-back, guards. |
| `internal/helper`, `cmd/spotexec-helper`, `internal/helperbin` | The remote helper (slots, sync, deps, exec, containers, snapshots) and its embedded builds. |
| `internal/transport`, `internal/protocol` | SSH ControlMaster transport and the framed stdio RPC. |
| `internal/provider` | `ssh`, `aws-spot` (hand-signed EC2/S3/ECR/SSM APIs), `sim`. |
| `internal/cloudinit` | First-boot user-data: watchdog, key, bootstrap. |
| `internal/daemon` | The optional pool manager. |
| `internal/manifest`, `internal/ignore`, `internal/dockerignore`, `internal/fsid`, `internal/depskey` | Worktree manifests, ignore semantics, safe path handling, dependency cache keys. |
| `internal/config`, `internal/hooks`, `internal/ui` | Config, agent hooks, output. |

## Design decisions

- **No backend.** Coordination lives in local `flock`ed state plus cloud tags, and every VM is responsible for its own death. Losing your laptop mid-run costs, at most, one `idle_timeout` of Spot time.
- **No rsync.** Sync streams a gzip'd tar of exactly the blobs the slot is missing, over the RPC channel. macOS ships openrsync, which differs from GNU rsync 3.x. Owning the bytes also makes the write-back baseline exact: what was uploaded is what the 3-way check compares against.
- **Git blob IDs (SHA-1), not a faster hash.** Clean tracked files reuse the index's IDs and are never read.
- **The watchdog is shell plus systemd,** installed by cloud-init before anything else runs. It exists before spotexec ever connects, and needs nothing from it.
- **Hardlink hydration is opt-in.** Shared inodes let one slot corrupt another's dependencies.
- **Not built yet:** `git_seed`, PTY allocation, parsed test-failure summaries.
