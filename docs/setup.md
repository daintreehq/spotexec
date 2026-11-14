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

1. **Runner config.** If `~/.config/spotexec/config.toml` doesn't exist, setup writes a starting one with a single `aws` runner in your AWS CLI's default region. An existing config is left as it is.
2. **AWS access**, for each `aws-spot` runner. Setup shows what it's about to do and asks before changing anything, then:
   - creates an IAM user for this machine, `spotexec-<hostname>` (path `/spotexec/`), with only [spotexec's permissions](aws.md#3-grant-permissions) as an inline policy. The user is tagged with this machine's permanent ID, so two Macs that share a hostname each get their own user (the second gets a suffix), and a machine whose hostname changes keeps its user;
   - creates an access key for that user and saves it as the AWS CLI profile `spotexec` (`~/.aws/credentials`, mode 600, with the region in `~/.aws/config`). It waits until the key works and checks that nothing else in `~/.aws` overrides it, then deletes any older keys the user has;
   - sets `profile = "spotexec"` on your AWS runners;
   - prepares the account: it creates the EC2 Spot service-linked role and, if the runner's region has no default VPC (common in older accounts), the default VPC. That's AWS's standard network and costs nothing; `--no-vpc` skips it;
   - checks your Spot vCPU quota against what your runners may use, and prints the command to raise it if it's too low.

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

**AWS Spot, which scales to zero.** `spotexec setup` handles credentials and permissions. The [AWS guide](aws.md) covers choosing a region, quotas, networking and doing each step by hand. VMs boot plain Ubuntu 24.04, so give them your toolchain: install it with `bootstrap_script` / `user_bootstrap_script` (see [configuring the runner](aws.md#7-configure-the-runner)), or per repository with a [project container](environments.md#project-containers) (`spotexec container init`).

```toml
version = 1
default_pool = ["aws"]

[runners.aws]
type = "aws-spot"
profile = "spotexec"   # set by `spotexec setup`
region = "us-east-2"   # always set it: the AWS profile's region is not used
instance_types = ["c7a.xlarge", "c6a.xlarge"]   # tried in order on capacity errors
max_slots = 2          # concurrent jobs per VM
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

Machines are set up four at a time, and each one's output is printed as a block when it finishes. For each machine, over one SSH connection (any SSH destination works, including `user@host` and `~/.ssh/config` aliases; key login is easiest, but a password prompt is fine):

1. **Installs spotexec.** With Homebrew there, it runs `brew install daintreehq/tap/spotexec`, or `brew upgrade` if spotexec is already installed. Without Homebrew, or when Homebrew fails or doesn't end up on the latest release (for example because the machine's Xcode Command Line Tools are out of date), it downloads the latest release binary into `~/.local/bin` instead. An older copy in `~/.local/bin` is removed only once Homebrew's is installed and working. `--install brew|release|copy|none` picks the method; `copy` sends this machine's own binary, for a machine of the same OS and architecture. Setup then checks that the installed spotexec reports the version it meant to install before going on.
2. **Copies the runner config** to `~/.config/spotexec/config.toml` there. A different existing config is first saved as `config.toml.bak-<time>`.
3. **Runs `spotexec setup` there.** For configs with AWS runners, it uses your AWS sign-in here. The temporary credentials go over the SSH connection's input, so they never appear in a command line and are never saved there. That machine gets **its own** IAM user, `spotexec-<its hostname>`, and key, and the same checks as this one. It also gets the Claude Code hook if this machine has it.

Last, setup checks that a fresh login shell there finds both `spotexec` and `sx`, and runs the version it just installed. If not (say `~/.local/bin` isn't on `PATH`, or an old Homebrew copy that couldn't be upgraded comes first), it reports the machine as not ready, with the fix; the machine is still remembered, so `spotexec setup --all` checks it again.

These stay separate on each machine:
- **IAM users and keys.** Retiring or losing a machine means deleting just that one user ([revoking a machine](#revoking-a-machine)).
- **The SSH key and owner ID** in `~/.config/spotexec/`. Each machine launches and manages its own VMs, and its runs never touch another's; see [Known limits](architecture.md#known-limits). Baked images are shared ([baked images](environments.md#baked-images)), and `spotexec status -A` shows every machine's VMs.

## Managing your machines

Every machine set up from here is remembered in `~/.config/spotexec/machines.toml`.

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
spotexec status -A                      # every spotexec VM in the account, by machine
spotexec down -A --machine studio-02    # stop one machine's VMs (asks first)
```

`setup --list` and `--revoke` need an administrator's AWS sign-in; `status -A` and `down -A` work with the machine's own key.

## Credentials and keys

**Why a key at all?** AWS's interactive sign-ins expire: `aws login` lasts at most 12 hours, and IAM Identity Center sessions last 8 hours by default (an administrator can allow up to 90 days). spotexec signs in however your AWS profile does, so with those you sign in again whenever the session runs out. The key that `spotexec setup` creates never expires, and it can only do what spotexec needs:
- launch, tag and terminate instances, and read the network;
- use its own `spotexec-cache-*` S3 buckets and `spotexec/*` ECR repositories;
- create the Spot service-linked role.

**Upgrading spotexec.** A new version may need more permissions. Run `spotexec setup` while signed in as an administrator to refresh the policy of this machine's user; run it with your other hosts to update theirs. Without an admin session, setup still checks the key and says the policy wasn't refreshed.

**Rotating the key.** `spotexec setup --rotate` creates a new key, verifies it, saves it, then deletes the old one.

### Revoking a machine

```sh
spotexec setup --revoke studio-01       # or the IAM user name, spotexec-studio-01
```

This deletes the machine's access keys, its policy and its IAM user, so its key stops working at once. It also terminates the machine's VMs and removes it from the machine list. On the machine itself, you can then delete the `[spotexec]` section of `~/.aws/credentials` and `[profile spotexec]` in `~/.aws/config`.

By hand, the equivalent is:

```sh
U=spotexec-studio-01
for k in $(aws iam list-access-keys --user-name "$U" --query 'AccessKeyMetadata[].AccessKeyId' --output text); do
  aws iam delete-access-key --user-name "$U" --access-key-id "$k"
done
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
