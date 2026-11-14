# Setting up machines

Every machine you run `sx` on needs three things:
- spotexec installed;
- a runner config, `~/.config/spotexec/config.toml`, which says where commands run;
- for AWS runners, AWS credentials.

`spotexec setup` handles all three, on this machine and on any others you name. It's safe to run again at any time.

- [This machine](#this-machine)
- [Choosing runners](#choosing-runners)
- [More machines](#more-machines)
- [Managing your machines](#managing-your-machines)
- [Credentials and keys](#credentials-and-keys)
- [Options](#options)

## This machine

```sh
brew install daintreehq/tap/spotexec
brew install awscli            # for AWS runners
aws login                      # or aws sso login / aws configure: an identity that can manage IAM users
spotexec setup
```

1. **Runner config.** If `~/.config/spotexec/config.toml` doesn't exist, setup writes a starting one with a single `aws` runner in the region of the AWS profile it signs in with (`AWS_REGION` or `AWS_DEFAULT_REGION`, else that profile's `region` in `~/.aws/config`, else `us-east-1`), and says which it picked. An existing config is left as it is.
2. **AWS access**, for each `aws-spot` runner. Setup shows what it's about to do and asks before changing anything, then:
   - creates an IAM user for this machine, `spotexec-<hostname>` (path `/spotexec/`), with only [spotexec's permissions](aws.md#3-grant-permissions) as an inline policy. The user is tagged with this machine's permanent ID, so two Macs that share a hostname each get their own user (the second gets a suffix), and a machine whose hostname changes keeps its user. A `spotexec` profile copied from another machine (a synced `~/.aws`) is replaced with this machine's own user and key, leaving the other machine's key alone, unless you name that user with `--user`. That takes an administrator's sign-in; without one, setup stops and says so. (A key whose policy predates this check can't read its own user, so then only a run with an administrator's sign-in can tell.) If the profile signs in to a different AWS account from your admin session, setup stops before changing anything;
   - creates an access key for that user and saves it as the AWS CLI profile `spotexec` (`~/.aws/credentials`, mode 600, with the region in `~/.aws/config`). It waits until the key works and checks that nothing else in `~/.aws` overrides it, then deletes any older keys the user has;
   - sets `profile = "spotexec"` on your AWS runners;
   - prepares the account: it creates the EC2 Spot service-linked role and, if the runner's region has no default VPC (common in older accounts), the default VPC. That's AWS's standard network and costs nothing; `--no-vpc` skips it;
   - checks your Spot vCPU quota. New accounts get about 5 vCPUs, too few for one of the starter config's 8-vCPU VMs, so a starter config setup has just written switches to 4-vCPU VMs (`c7a.xlarge`…) instead, keeping `max_instances`. If the quota can't fit even one VM, setup says so, prints the command to raise it and, in a terminal, offers to request the increase with your AWS sign-in. If it only limits how many VMs run at once, setup notes that and prints the command. spotexec never falls back to on-demand instances unless you set `on_demand_fallback`.

   With runners in several regions, the network and quota checks cover each region.
3. **The Claude Code hook.** If Claude Code is on this machine, setup offers to install its hook for all your projects, so agents' heavy commands run on your runners (see [coding agents](agents.md#claude-code-hook)). The hook only acts in projects with a `.spotexec.toml`. `--no-hooks` skips this.
4. **Other machines.** In a terminal, setup then lists the machines it can find and asks which ones to set up too; see [More machines](#more-machines).
5. **A baked image.** If an AWS runner has no current [baked image](environments.md#baked-images) (none yet, or a stale one), setup offers to bake one. Without it, every new VM spends a minute or more installing its bootstrap (apt packages, Docker) before the first command runs. Baking takes about 10 to 15 minutes plus your bootstrap, on a short-lived on-demand VM, and every machine sharing the runner config then boots from it. `--yes` skips the offer unless you also pass `--bake`, which bakes without asking. You can always run `spotexec image bake` later.

From then on spotexec uses its own key. Your `aws login` or SSO session can expire without it noticing, so you sign in again only to change the setup.

```console
$ spotexec setup
✓ runner config /Users/me/.config/spotexec/config.toml
Setting up AWS access for aws (region us-east-1)…

Signed in to AWS account 123456789012 as arn:aws:iam::123456789012:user/me. This will:
  • create (or update) IAM user spotexec-studio-02 with only spotexec's permissions (see `spotexec setup --print-policy`)
  • save an access key for it as AWS profile "spotexec" in ~/.aws/credentials (if it hasn't one already)
  • set profile = "spotexec" on runner(s) aws
Continue? [Y/n]
  created IAM user spotexec-studio-02
  attached the spotexec policy
  created access key AKIA…
  saved it as AWS profile spotexec (/Users/me/.aws/credentials)
  verified: the key signs in and can reach EC2 in us-east-1
✓ AWS profile spotexec → arn:aws:iam::123456789012:user/spotexec/spotexec-studio-02 (key AKIA…; it doesn't expire)
✓ set profile = "spotexec" on aws in /Users/me/.config/spotexec/config.toml
✓ Spot vCPU quota in us-east-1: 640 (runners need up to 32)
```

Next, in each repository: `spotexec init`, then `sx <command>`. `spotexec doctor` checks everything end to end.

Before a demo, do one warm-up `sx` run. The first run boots a VM, which takes a minute or more (plus the bootstrap unless the image is baked); later runs reuse it until it has been idle for `idle_timeout`.

## Choosing runners

A **runner** is either one **SSH host** or an elastic **fleet** of Spot **instances** (VMs). A **pool** is the ordered list of runners a command may use, and each machine offers `max_slots` isolated **slots**, one job per slot. Edit `~/.config/spotexec/config.toml` so `default_pool` lists only runners you actually have, then run `spotexec setup` again (and again for other machines, so they get the change). The [configuration reference](configuration.md) lists every key.

**An SSH machine you already own.** Non-interactive key login must already work, and the host key must be in `~/.ssh/known_hosts`, because spotexec runs `ssh -o BatchMode=yes`. Check with `ssh -o BatchMode=yes me@192.168.1.50 true`. Commands run under `bash` with a login profile (`~/.bash_profile`, falling back to `~/.profile`; not `~/.bashrc` or `~/.zprofile`), so your toolchain (`pnpm`, `go`, `cargo`, …) must be on `PATH` there, set by `remote_path_setup`, or come from a [project container](environments.md#project-containers).

```toml
version = 1
default_pool = ["homelab"]

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]   # same machine, two routes
max_slots = 4
```

**AWS Spot, which scales to zero.** `spotexec setup` handles credentials and permissions. The [AWS guide](aws.md) covers choosing a region, quotas, networking and doing each step by hand. VMs boot plain Ubuntu 24.04, so each repository brings its toolchain in a [project container](environments.md#project-containers), which `spotexec init` sets up when it detects one. Installing it on the VMs with `bootstrap_script` / `user_bootstrap_script` instead (see [configuring the runner](aws.md#7-configure-the-runner)) needs `init --no-container`.

```toml
version = 1
default_pool = ["aws"]

[runners.aws]
type = "aws-spot"
profile = "spotexec"   # set by `spotexec setup`
region = "us-east-2"   # always set it: the AWS profile's region is not used
instance_types = ["c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"]   # 8 vCPUs; tried in order on capacity errors
max_slots = 4          # concurrent jobs per VM
max_instances = 4      # fleet ceiling
idle_timeout = "10m"   # the VM powers itself off after this long without work
```

**Both.** `default_pool = ["homelab", "aws"]`, with `priority = 10` on `homelab` and `priority = 20` on `aws`, fills the homelab first and sends the overflow to AWS. Lower numbers run first.

## More machines

Name the machines, and setup runs on each of them after this one:

```sh
spotexec setup studio-01.local studio-02.local me@build-box
```

Or run `spotexec setup` on its own in a terminal and pick from the machines it finds: the ones set up from here before, Macs with Remote Login on your network (Bonjour, or avahi on Linux), and the `Host` entries in `~/.ssh/config`. Names that start with `-` or contain spaces or control characters are refused (and left out of the list), so nothing on the network can pass options to `ssh`.

```console
Set up other machines too? spotexec installs itself there over SSH, copies this config, and gives each machine its own AWS key.
Found on your network, in ~/.ssh/config, and set up before:
   1. studio-01.local   (set up Nov 14, spotexec 0.3.0)
   2. studio-02.local
   3. mini-01.local
Numbers, `all`, or host names, separated by spaces (Enter to skip): 1 2
```

Setup first connects to each machine in turn, so any SSH prompt (a new host key, a password) comes one machine at a time. It then sets the machines up four at a time over those connections, without prompting again, and prints each one's output as a block when it finishes. Any SSH destination works, including `user@host` and `~/.ssh/config` aliases; key login is easiest, but a password prompt is fine. For each machine:

1. **Installs spotexec.** With Homebrew there, it runs `brew install daintreehq/tap/spotexec`, or `brew upgrade` if spotexec is already installed. Without Homebrew, or when Homebrew fails or doesn't end up on the latest release (for example because the machine's Xcode Command Line Tools are out of date), it downloads the latest release binary into `~/.local/bin` instead, checking the archive against the release's `checksums.txt` before it replaces anything (a mismatch leaves the existing install as it was). A machine already on the latest release is left alone. An older copy in `~/.local/bin` is removed only once Homebrew's is installed and working. `--install brew|release|copy|none` picks the method; `copy` sends this machine's own binary, for a machine of the same OS and architecture. Setup then checks that the installed spotexec reports the version it meant to install before going on.
2. **Copies the runner config** to `~/.config/spotexec/config.toml` there. A different existing config is first saved as `config.toml.bak-<time>`.
3. **Runs `spotexec setup` there.** For configs with AWS runners, it uses your AWS sign-in here, and only that: never the other machine's own AWS login. The temporary credentials go over the SSH connection's input, so they never appear in a command line and are never saved there. Without a sign-in here, a machine whose key still works is updated (only its policy isn't refreshed), and one that needs a new user or key is reported, with the command to run once you've signed in. That machine gets **its own** IAM user, `spotexec-<its hostname>`, and key, and the same checks as this one. It also gets the Claude Code hook if Claude Code is installed there, unless you passed `--no-hooks` or turned the hook down here.

Last, setup checks that a fresh login shell there finds both `spotexec` and `sx`, and runs the version it just installed. If not (say `~/.local/bin` isn't on `PATH`, or an old Homebrew copy that couldn't be upgraded comes first), it reports the machine as not ready, with the fix; the machine is still remembered, so `spotexec setup --all` checks it again.

These stay separate on each machine:
- **IAM users and keys.** Retiring or losing a machine means deleting just that one user ([revoking a machine](#revoking-a-machine)).
- **The SSH key and owner ID** in `~/.config/spotexec/`. Each machine launches and manages its own VMs, and its runs never touch another's; see [Known limits](architecture.md#known-limits). Baked images are shared ([baked images](environments.md#baked-images)), and `spotexec status -A` shows every machine's VMs.

## Managing your machines

Every machine set up from here is remembered in `~/.config/spotexec/machines.toml`, with the machine each SSH destination reached (its spotexec machine ID, which also tags its IAM user). Setting a machine up again under another name, say an alias, replaces its old entry, so `--all` sets each machine up once.

**Update them all** after editing the config or upgrading spotexec here:

```sh
spotexec setup --all
```

Each machine gets the new config, its spotexec is upgraded to the latest release, its IAM policy is refreshed, and its working key is kept. Creating a key for a new machine needs a current AWS sign-in here (run `aws login` first if it has expired); machines that already have a key are updated without one.

**See them all:**

```console
$ spotexec setup --list
IAM USER                           HOST       KEY                KEY CREATED       LAST USED         VMS
spotexec-studio-01                 studio-01  AKIA…AJTC          2026-11-14 08:01  2026-11-14 11:20  2
spotexec-studio-02                 studio-02  AKIA…BJS3          2026-11-14 08:19  2026-11-14 11:18  1
spotexec-gregs-mac (this machine)  gregs-mac  AKIA…YEAW          2026-11-14 07:55  2026-11-14 11:21  0
```

```sh
spotexec status -A                      # every spotexec VM (any machine, any state), by machine
spotexec down -A --machine studio-02    # stop one machine's VMs (asks first)
```

`setup --list` and `--revoke` need an administrator's AWS sign-in; `status -A` and `down -A` work with the machine's own key. "The account" there means the regions and profiles your AWS runners use: both commands include stopped VMs and `image bake` builders, name the regions and accounts they searched, terminate each VM through the profile that found it, and exit non-zero if a region couldn't be listed or a VM couldn't be terminated.

## Credentials and keys

**Why a key at all?** AWS's interactive sign-ins expire: `aws login` lasts at most 12 hours, and IAM Identity Center sessions last 8 hours by default (an administrator can allow up to 90 days). spotexec signs in however your AWS profile does, so with those you sign in again whenever the session runs out. The key that `spotexec setup` creates never expires, and it can only do what spotexec needs:
- launch, tag and terminate instances, and read the network;
- use its own `spotexec-cache-*` S3 buckets and `spotexec/*` ECR repositories;
- create the Spot service-linked role.

**Upgrading spotexec.** A new version may need more permissions. Run `spotexec setup` while signed in as an administrator to refresh the policy of this machine's user; run it with your other hosts to update theirs. Without an admin session, setup still checks the key and says the policy wasn't refreshed.

**Rotating the key.** `spotexec setup --rotate` creates a new key, verifies it, saves it, then deletes the old one. If the user already has two keys (IAM's limit), it first deletes the one this machine's profile doesn't use, so a failed rotation never leaves the machine without a working key.

### Revoking a machine

```sh
spotexec setup --revoke studio-01       # or the IAM user name, spotexec-studio-01
```

The machine can be named by its IAM user (an exact name always selects that user), by a destination you set it up with (`builder`, `me@studio-01.local`), or by its host name. When two machines share a host name, name the IAM user. This deactivates the machine's access keys, so its key stops working at once, then terminates the machine's VMs (stopped ones and `image bake` builders too) in the regions your AWS runners use, and only then deletes its keys, policy and IAM user and removes every machine-list entry that reached it, aliases included. Entries recorded by spotexec versions before machine IDs, which it can't place, are listed for you to check. If a region can't be searched or a VM can't be terminated, it says so, exits non-zero and keeps the IAM user and the machine-list entries, so running `--revoke` again finishes the cleanup. On the machine itself, you can then delete the `[spotexec]` section of `~/.aws/credentials` and `[profile spotexec]` in `~/.aws/config`.

By hand, with an administrator's sign-in: deactivate the keys first, so the machine can't launch anything more; then terminate its VMs (`spotexec down -A --machine studio-01`, which searches your AWS runners' regions) and wait until `spotexec status -A` shows none of them; only then delete the keys, policy and user:

```sh
U=spotexec-studio-01
KEYS=$(aws iam list-access-keys --user-name "$U" --query 'AccessKeyMetadata[].AccessKeyId' --output text)
for k in $KEYS; do aws iam update-access-key --user-name "$U" --access-key-id "$k" --status Inactive; done
spotexec down -A --machine studio-01 --yes && spotexec status -A    # repeat until none of its VMs are left
for k in $KEYS; do aws iam delete-access-key --user-name "$U" --access-key-id "$k"; done
aws iam delete-user-policy --user-name "$U" --policy-name spotexec
aws iam delete-user --user-name "$U"
```

### Without a long-lived key

To keep no key on disk, skip the AWS step (`spotexec setup --skip-aws`) and sign in with IAM Identity Center ([AWS guide, step 2A](aws.md#2-sign-in)), with the [policy](aws.md#3-grant-permissions) on your permission set. spotexec then works for as long as your SSO session lasts.

## Options

| Flag | Effect |
| --- | --- |
| `[host...]` | Also set up these machines over SSH. |
| `--all` | Also update every machine set up from here before. |
| `--list` | List every machine's IAM user, key, last use and VMs, and exit. |
| `--revoke HOST` | Delete a machine's IAM user and keys and terminate its VMs, and exit. |
| `--no-hooks` | Don't install the Claude Code hook. |
| `--yes`, `-y` | Don't ask for confirmation, and don't offer to set up other machines or bake an image. |
| `--bake` | Bake an image for AWS runners without a current one, without asking (also with `--skip-aws`). A failed bake fails setup. |
| `--install auto\|brew\|release\|copy\|none` | How to install spotexec on other machines (default `auto`: Homebrew if it's there, else the release binary). |
| `--admin-profile NAME` | The AWS profile to create users with (default: `AWS_PROFILE`, then `default`). |
| `--profile NAME` | The AWS profile to save the key as (default `spotexec`). |
| `--user NAME` | This machine's IAM user name (default `spotexec-<hostname>`). |
| `--rotate` | Replace the access key even if it works. |
| `--no-vpc` | Don't create a default VPC when the region has none. |
| `--skip-aws` | Only write (and copy) the runner config. |
| `--print-policy` | Print the IAM policy and exit. |
