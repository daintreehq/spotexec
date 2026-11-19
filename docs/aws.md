# AWS setup

This is the full path from an AWS account to a working Spot fleet.

**The fast path:** sign in once as an administrator and run [`spotexec setup`](setup.md). It does steps 2, 3 and 7 for you (a dedicated IAM user, its policy and a non-expiring key, saved as the `spotexec` profile), prepares the account (the Spot service-linked role, and a default VPC if the region has none) and checks your quota. It can then set up your other machines over SSH. This page explains each part and how to do it by hand.

1. [Install the AWS CLI](#1-install-the-aws-cli)
2. [Sign in](#2-sign-in)
3. [Grant permissions](#3-grant-permissions)
4. [Choose a region](#4-choose-a-region)
5. [Check your Spot vCPU quota](#5-check-your-spot-vcpu-quota)
6. [Check the network](#6-check-the-network)
7. [Configure the runner](#7-configure-the-runner)
8. [Verify end to end](#8-verify-end-to-end)

## 1. Install the AWS CLI

spotexec signs its own AWS requests, so plain access keys work without the CLI. Install AWS CLI v2 anyway if you sign in with IAM Identity Center (SSO), `aws login` or an assume-role profile. The CLI resolves those, and you will want it for the quota and policy commands below.

```sh
brew install awscli        # macOS; other platforms: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
aws --version              # aws-cli/2.9 or later (2.32+ for `aws login`)
```

## 2. Sign in

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

**C. IAM user access keys.** The simplest option for a personal account, and what `spotexec setup` does for you. Use a dedicated user with only the [policy below](#3-grant-permissions), not your root user. To do it by hand, create the user and key from an admin session (or in the IAM console):

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

## 3. Grant permissions

`spotexec setup` attaches this policy to the user it creates. To grant it yourself, attach it to the identity you signed in with; `spotexec setup --print-policy > spotexec-policy.json` saves it.

The policy is scoped to what spotexec creates:
- Reads (`ec2:Describe*`) are unconditional.
- Every EC2 call that changes an existing resource (terminating or stopping an instance, imaging it, deregistering an AMI, deleting a snapshot, adding a rule to a security group) only works on resources tagged `spotexec-owner`.
- Tags can only be applied while spotexec creates the resource (`ec2:CreateAction`), so the key can't tag an unrelated instance to get around the conditions. `RunInstances` is allowed only when the new instance and its volumes are requested with the `spotexec-owner` tag; the image, subnet and security groups it launches with needn't be spotexec's.
- S3 and ECR access is limited to `spotexec-cache-*` buckets (and their objects) and `spotexec/*` repositories. A bucket you name with `cache_bucket` isn't covered: grant `s3:ListBucket` on it and `s3:GetObject` and `s3:PutObject` on its objects in a policy of your own, or spotexec reports access denied on its first `[deps]` run.
- `iam:GetUser` only covers the key's own user under `/spotexec/`, so setup can check which machine it belongs to.

So a leaked key can launch tagged VMs and manage spotexec's own resources, but can't terminate, image or open up anything else in the account.

The S3 statement (the [dependency cache](environments.md#dependency-cache); without it, set `cache_bucket = "off"` on the runner), the ECR statements (Dockerfile-built [containers](environments.md#project-containers)) and `servicequotas:GetServiceQuota` (`doctor`'s quota check and the pre-launch quota check) are optional. Everything else is needed to launch and clean up VMs and to [bake images](environments.md#baked-images).

The policy is attached inline, and an IAM user's inline policies are limited to 2,048 characters, which is why some statements group several actions. Its ARNs are in the commercial partition (`arn:aws:`). In GovCloud (US), setup attaches it with `arn:aws-us-gov:` instead, as `--print-policy` prints it when your runners (or your admin profile's region) are in `us-gov-*`. Re-running `spotexec setup` replaces the policy with the current version's. It also tags any `spotexec-ssh*` security group an older spotexec made without the tag. Until then, a VM can still launch into such a group if it already allows SSH from `ssh_cidr`, but spotexec can't add a rule to it.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": [
        "ec2:Describe*", "ec2:CreateSecurityGroup", "ecr:GetAuthorizationToken", "servicequotas:GetServiceQuota"
      ], "Resource": "*" },
    { "Effect": "Allow", "Action": "ec2:RunInstances",
      "NotResource": ["arn:aws:ec2:*:*:instance/*", "arn:aws:ec2:*:*:volume/*"] },
    { "Effect": "Allow", "Action": "ec2:RunInstances",
      "Resource": ["arn:aws:ec2:*:*:instance/*", "arn:aws:ec2:*:*:volume/*"],
      "Condition": { "Null": { "aws:RequestTag/spotexec-owner": "false" } } },
    { "Effect": "Allow", "Action": "ec2:CreateTags", "Resource": "*",
      "Condition": { "StringEquals": { "ec2:CreateAction": ["RunInstances", "CreateSecurityGroup", "CreateImage"] } } },
    { "Effect": "Allow", "Action": [
        "ec2:TerminateInstances", "ec2:AuthorizeSecurityGroupIngress",
        "ec2:StopInstances", "ec2:CreateImage", "ec2:DeregisterImage", "ec2:DeleteSnapshot"
      ], "Resource": "*", "Condition": { "Null": { "aws:ResourceTag/spotexec-owner": "false" } } },
    { "Effect": "Allow", "Action": ["ec2:AuthorizeSecurityGroupIngress", "ec2:CreateImage"], "Resource": [
        "arn:aws:ec2:*:*:security-group-rule/*", "arn:aws:ec2:*::image/*", "arn:aws:ec2:*::snapshot/*"
      ] },
    { "Effect": "Allow", "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:*::parameter/aws/service/canonical/*" },
    { "Effect": "Allow", "Action": "iam:GetUser", "Resource": "arn:aws:iam::*:user/spotexec/${aws:username}" },
    { "Effect": "Allow", "Action": "iam:CreateServiceLinkedRole", "Resource": "*",
      "Condition": { "StringEquals": { "iam:AWSServiceName": "spot.amazonaws.com" } } },
    { "Effect": "Allow", "Action": [
        "s3:CreateBucket", "s3:*LifecycleConfiguration", "s3:ListBucket",
        "s3:GetObject", "s3:PutObject"
      ], "Resource": "arn:aws:s3:::spotexec-cache-*" },
    { "Effect": "Allow", "Action": [
        "ecr:Describe*", "ecr:CreateRepository", "ecr:TagResource", "ecr:PutLifecyclePolicy",
        "ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
        "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"
      ], "Resource": "arn:aws:ecr:*:*:repository/spotexec/*" }
  ]
}
```

For an IAM user (option C), run this with an admin identity:

```sh
ARN=$(aws iam create-policy --policy-name spotexec --policy-document file://spotexec-policy.json --query Policy.Arn --output text)
aws iam attach-user-policy --user-name spotexec --policy-arn "$ARN"
```

For IAM Identity Center, add the JSON as an inline policy on the permission set you sign in with. For a role, attach it to the role.

**The Spot service-linked role.** The account needs the service-linked role `AWSServiceRoleForEC2Spot` before its first Spot request. The `iam:CreateServiceLinkedRole` statement lets that request create it. If your identity can't create roles, have an admin run this once (an "already exists" error is fine):

```sh
aws iam create-service-linked-role --aws-service-name spot.amazonaws.com
```

## 4. Choose a region

A runner's region comes from, in order:
1. its `region` key;
2. `AWS_REGION`;
3. `AWS_DEFAULT_REGION`;
4. `us-east-1`.

The `region` in your `~/.aws/config` profile is **not** used at run time, so set `region` on the runner explicitly. When `spotexec setup` (or `spotexec init --global`) writes a new config, it fills `region` in the same order the AWS CLI uses: `AWS_REGION`, `AWS_DEFAULT_REGION`, then the `region` of the profile setup signs in with (`--admin-profile`, else `AWS_PROFILE`, else `default`; SSO profiles included), read from `~/.aws/config` (or `~/.aws/credentials`) without needing the AWS CLI, and `us-east-1` when none is set. It prints which one it used.

Pick the region that is:
- **Closest to you.** Every run makes several SSH round trips (handshake, sync, streaming output, write-back). A nearby region keeps small commands under a second of overhead.
- **Stocked with your instance types.** The spotexec policy allows these read-only `Describe*` lookups (add `--profile spotexec`). For example, check that the region offers the starter config's types:
  ```sh
  aws ec2 describe-instance-type-offerings --region us-east-2 \
    --filters Name=instance-type,Values=c7a.2xlarge,m7a.2xlarge,c6a.2xlarge --query 'InstanceTypeOfferings[].InstanceType'
  ```
- **Cheap and roomy for Spot.** Compare current prices, and ask AWS how likely a request is to be filled (scores 1–10; `get-spot-placement-scores` isn't in the spotexec policy, so run it from an admin session):
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

### Launch on the cheapest instance

Each time the fleet needs a new VM, spotexec compares the current Linux Spot price of every instance type the runner may use in every availability zone of its region, and launches the cheapest. (With `vcpus`, or several regions, the ranking also counts what the price leaves out, such as a slower generation or a cold region: see [Rank by the cost of the run](#rank-by-the-cost-of-the-run).) If AWS refuses it for capacity, it moves on to the next-cheapest option, and that zone and type are skipped for a minute (longer if it keeps failing). When a VM gets a Spot interruption notice, its zone and type rank last for 30 minutes (each further VM reclaimed there doubles that, up to 4 hours), so its replacement and later launches go to the next-cheapest option instead of back into a pool that keeps reclaiming them; it is still used if every other option fails. A pinned `subnet_id` fixes the zone, so only that zone's prices count; otherwise each launch goes to the default VPC's subnet in the chosen zone. `max_price` still caps the bid, and options above it are skipped. When every option is above it, with no VM running for the run to wait for, the run fails at once (exit 125) with the cap and the cheapest current price, instead of waiting out its queue timeout. Prices are cached for 15 minutes; `spotexec prices <runner>` shows the ranking, marks where the next VM will go and which options are skipped or held back (and why), and `spotexec doctor` names it.

Equal prices go to the type listed first in `instance_types`. To keep the listed order as a preference instead (say, a faster type first and a cheaper one only as a fallback), set `launch_order = "listed"`: each type is then tried in its cheapest zone first, before the next type. If prices can't be read (an older IAM policy without `ec2:Describe*`), a single-region runner with a fixed `instance_types` list tries them in the listed order as it used to (with `regions`, or `vcpus`, the launch fails instead); re-run `spotexec setup` to update the policy.

With `on_demand_fallback`, on-demand is the last resort, after every Spot option. spotexec doesn't look up on-demand prices (that needs the AWS Pricing API, another permission and endpoint), so it tries the types in their Spot-price order, a fair stand-in: within one size, on-demand and Spot prices tend to rank the same way. With `max_price`, a type whose Spot price is above it is never launched on-demand either (a Spot price is never above the on-demand price, so its on-demand price is above the cap too), and types without a Spot price are left out: when every Spot option is above `max_price`, the launch fails with the cap rather than falling back. `max_price` caps only the Spot bid, so an on-demand VM of a type priced under it on Spot can still cost more than `max_price`.

#### Describe the VM instead of naming types

Rather than picking instance types by hand, give the size and let spotexec consider every current instance type in the region that fits:

```toml
[runners.aws]
type = "aws-spot"
region = "us-east-1"
vcpus = 8              # exactly 8 vCPUs per VM
min_memory_gib = 32    # at least 32 GiB
max_slots = 4
```

spotexec lists the region's instance types once a day (EC2 `DescribeInstanceTypes`, cached in the runner's state directory) and keeps those that:
- have exactly `vcpus` vCPUs, the runner's `arch` (default `amd64`) and at least `min_memory_gib`;
- are current generation, and at least generation `min_generation` (default 6: m5, c5 and r5 are still "current" to AWS, but markedly slower per vCPU, and often the cheapest);
- can run as Spot, aren't burstable (`t*`), bare metal or Flex (`m7i-flex` and the like don't sustain full CPU), and have no GPU or other accelerator;
- count against the standard Spot vCPU quota (A, C, D, H, I, M, R, T, Z families), so the whole fleet fits one quota.

Every type left is ranked by [the expected cost of the run](#rank-by-the-cost-of-the-run): its current Spot price, plus the VM time and waiting a slower type adds (until the runner's own runs have measured them, each generation counts as 10% faster per vCPU, so with the default `wait_cost` an m7a at $0.055/h beats an m6i at $0.050/h). Set `launch_order = "cheapest"` to rank by price alone. Types with local NVMe storage (`m6id`, `c7gd`) stay in: spotexec doesn't use the disk, but they're more capacity to draw on and are only chosen when they rank first.

To narrow the choice, list patterns in `instance_types` (only types matching one are used, so `["m*", "r*"]` leaves out compute-optimized types), or leave some out with `exclude_instance_types = ["m6i.*", "*n.*"]`. `*` matches any run of characters. Set `min_generation = 7` for the newest generations only (on AMD, that is also one physical core per vCPU).

Why an exact vCPU count, not a range: slots and `$SPOTEXEC_CPUS` come from the vCPUs, so with one count every VM in the fleet gets the same slots and the same CPU share per job, whatever type it lands on. Memory varies (each VM has at least `min_memory_gib`), and only ever upwards. Ranked by price per VM, a range would almost always pick its smallest size anyway. For two sizes, define two runners.

All the types share the runner's `arch`, because the Ubuntu image, a baked AMI and project containers are built for one architecture. For Graviton, set `arch = "arm64"`.

### Let spotexec pick the region

Instead of `region`, a runner can list several regions, or allow them all:

```toml
[runners.aws]
type = "aws-spot"
regions = ["all"]          # every region enabled in the account
# regions = ["us-east-1", "us-east-2", "eu-north-1"]
instance_types = ["c7a.2xlarge", "c6a.2xlarge", "m7a.2xlarge"]
max_instances = 4          # across all regions together
```

To keep `"all"` but leave some regions out (one whose Spot VMs are reclaimed often, one too far away, or one ruled out for data residency), name them in `exclude_regions`:

```toml
regions = ["all"]
exclude_regions = ["ap-south-2", "me-central-1"]
```

A region enabled later is still picked up, and `spotexec prices` and `doctor` list the excluded ones under "Not used". VMs already running in a region you exclude keep their runs and are still listed and torn down; new VMs go elsewhere. (With a list of regions, just leave the region out.)

Each time the fleet needs a new VM, spotexec compares every instance type the runner may use (listed, or [described by size](#describe-the-vm-instead-of-naming-types)) in every availability zone of those regions, and launches the one with the lowest [expected cost of the run](#rank-by-the-cost-of-the-run): its current Linux Spot price, plus the waiting a cold region, a farther region or (with `vcpus`) a slower type adds. A region the fleet has used lately keeps new VMs until another region's saving outweighs moving there. If AWS refuses the first option, spotexec moves on to the next:
- **No capacity** for that type in that zone: skipped for a minute, then longer if it keeps failing.
- **Spot interruption** of a VM in that type and zone (not a refusal, but the same idea): it ranks last for 30 minutes, longer if more VMs there are reclaimed, as above.
- **Quota exhausted**: that instance size and larger are skipped in that region; smaller types, and types under another quota, can still launch there. When quota is all that stopped every option, spotexec says so and holds new launches for a few minutes, or until one of the fleet's VMs ends, instead of retrying.
- **Region not usable** (no default VPC, launches blocked by permissions): the whole region is skipped for 10 minutes.
- **Region not enabled** in the account (or misspelled), or in `exclude_regions`: left out entirely, and `spotexec prices` lists it under "Not used" with the reason. spotexec rechecks which regions are enabled once a day, or now with `spotexec prices --refresh`.
- **Anything that would fail everywhere** (a bad bootstrap script, invalid parameters): the launch stops.

`spotexec prices` shows the ranking, with what each factor added, and marks where the next VM will go. Prices are cached for 15 minutes. VMs that are already running stay where they are; a price change only affects new launches.

What changes with several regions:
- **Distance.** The cheapest region may be on another continent, and every run makes several SSH round trips. The ranking counts the extra round trips, but if latency matters more than that, list nearby regions instead of `"all"`.
- **Quota.** Each region has its own Spot vCPU quota, and a region the account has never used is often at 5 vCPUs. A launch there fails over to the next region. `spotexec doctor` checks the cheapest few regions.
- **Network and images.** spotexec uses each region's default VPC and its own security group there, and the region's Ubuntu image, or a bake if you made one there (`spotexec image bake <runner> --region <r>`; bakes in different regions can run at the same time, one per region). That is why `subnet_id`, `security_group_ids` and `ami-…` IDs aren't allowed with `regions`. (A list of one region is just `region`, and those settings work as usual.) A region without a default VPC is skipped; `spotexec setup` creates one for each listed region (not for `"all"`).
- **Caches.** The S3 dependency cache (`cache_bucket = "auto"`: a bucket per region) and ECR images are per region, so the first run in a new region starts cold. The ranking counts that, so a fleet doesn't hop regions for a small saving. A named `cache_bucket` lives in one region, so it isn't allowed with `regions`.
- **Permissions.** Comparing prices needs `ec2:DescribeRegions` and `ec2:DescribeSpotPriceHistory`, and `vcpus` needs `ec2:DescribeInstanceTypes`, all covered by the policy's `ec2:Describe*`. If your IAM user's policy predates that, re-run `spotexec setup` to update it.

`spotexec status` shows each VM's region. `spotexec down` terminates VMs in every region the runner uses, including regions since removed from the config. Going back to a single `region`, or dropping regions from the list, is safe too: until the VMs in the other regions are gone, spotexec still lists and terminates them, while new VMs launch only in the regions still configured. Those VMs take no new runs (`spotexec status` shows them as `outdated`) and terminate when idle. spotexec records where it launched in `placement.json` in the runner's state directory. If that file is lost, the next listing checks every region the runner is configured for once; VMs in a region no longer in the config are then left to their own idle shutdown and `max_lifetime`.

To prefer one region and send only overflow elsewhere, define one runner per region and order them with `priority` instead.

### Rank by the cost of the run

The cheapest hour isn't always the cheapest run. A region without the runner's dependency cache, container image or bake makes the first runs there install, build and boot from scratch, an older generation does the same work more slowly, and a far region adds round trips to every run. At a few cents an hour for an 8-vCPU Spot VM, a minute of someone (or an agent) waiting for results is usually worth more than the price gaps between regions.

So by default (`launch_order = "run_cost"`) each option is scored by what an hour of work there is expected to cost, in USD: its Spot bill, plus the waiting the option adds, valued at `wait_cost` per hour (default `$1`):

| Factor | What spotexec observes | What it adds |
| --- | --- | --- |
| Spot price | the current price in that zone | the hourly price |
| Speed (`vcpus` runners) | how long the same commands' processes took on each type, from the runner's commands that exited 0 on its VMs in the last 30 days (the latest 30 of a command on a type, for up to 64 commands); otherwise the type's generation, from its name (m7a: 7) | a slower type adds the extra time its hour of work takes, for the VM and the waiting. Once a command (the same project, directory, command line, environment and `exclusive`) has run 5 times on each of two types, their typical times (geometric means) compare them, each weighed by how many VMs its runs were on and how closely those agree: jobs share a VM's CPUs, so a run beside busy neighbours takes longer, and a few VMs' runs, or runs that scatter that way, barely move a type from its generation's speed, while many VMs' consistent ones place it where they say (a type measured at 0.70x another's speed, VM after VM, counts as close to that). Each VM counts once, as the geometric mean of its runs. Whatever the runs say, measured speeds move a type's charge at most 3 hours of its Spot price from what its generation alone says, since every VM of one type can happen to be busy: a type truly slower still pays several times its price for it. The newest generation measured keeps its speed by generation. Types never measured that way count each generation as 10% faster per vCPU than the one before. `spotexec prices` marks each type's speed as measured or assumed |
| Cold region (several regions) | what the region holds, account-wide: a usable [bake](environments.md#baked-images) of the runner; the run's dependency archive under its exact key in the region's cache bucket; the project's image under its exact tag in the region's ECR repository | of the VM and of waiting, once per VM: 1 minute for booting without a bake; for installing dependencies without the archive and building the image, the longest install and build of the project measured on VMs of the runner in the last 30 days (at most an hour each), or 2 and 1 minutes when none was; nothing for a part the run doesn't need |
| Distance (several regions) | the TCP connect time to the region's EC2 endpoint, measured whenever prices are fetched | 300 round trips (about 30 runs of 10) at the extra time over the nearest region, for the VM and the waiting |
| Failures | capacity refusals of a launch in that zone and type, and Spot interruption notices to VMs there within an hour of their launch | 3 minutes of the VM and waiting for each, counting half as much every 2 hours (at most 16 at a time): a pool that keeps refusing falls behind an older generation or another region for longer the more it fails, and is tried again as its failures fade. It isn't skipped, so it is still used when everything ahead of it fails |
| Interruptions | Spot interruption notices to the runner's VMs | the zone and type rank last for a while (see [above](#launch-on-the-cheapest-instance)) |

The fleet moves to a cold region only when its saving over an hour of work pays for the cold start, `(price + wait_cost) × 4 minutes` when nothing is there and the project's install and build haven't been measured lately: with the default `wait_cost`, about $0.07 for an 8-vCPU VM. Against a warm region at $0.100/h, a cold one at $0.096/h (4% cheaper) doesn't move it; one at $0.020/h does. Equal scores go in price order, as `launch_order = "cheapest"` would.

Because the bake, the cache and the registry are the account's, every machine sharing the account sees the same warmth: `spotexec image bake <runner> --region <r>` makes launches prefer `<r>` for the boot part at once on the machine that baked, and within 10 minutes on the others. A run's launches check its own project's keys, so a region that ran VMs for another project, or before the lockfile or Dockerfile changed, isn't warm for what it doesn't hold. Where the project can't be checked (the pool manager's launches for no run in particular, `spotexec prices` outside a project, an image pulled from elsewhere, or a lookup that failed), its parts count as warm where a VM of this runner has been up for at least 10 minutes in the last 7 days, as seen by this machine's listings. Lookups are kept in `placement.json` for 10 minutes (a failed one for a minute).

A project whose image compiles tools from source, or whose install rebuilds native modules, can take 10 or 20 minutes for each, so a run's own image builds and dependency installs (those that actually built or installed, not a pull or a restore) are kept in the runner's `placement.json`, under the repository, and its next launches price those parts by the longest. Quick rebuilds that reused Docker's layer cache, or installs that reused a download cache, don't push out a long one before it is 30 days old. The pool manager's launches and `spotexec prices` outside a project use the defaults. The other constants are assumptions, not measurements of your project, and `spotexec prices --json` reports them with every score (`run_cost_model`, where `cold_deps_estimate` and `cold_image_estimate` say whether each part was measured, when the longest was taken, and whether it was capped at an hour), and with each option which parts of a cold start it pays and why (`run_cost.cold_parts`); the text output names the source of each part's minutes under the table.

- **`wait_cost`** sets what an hour of waiting is worth. A batch runner whose results nobody waits on can set `wait_cost = 0`: only the Spot bill counts, including the extra VM time of a slower type or a cold start. An interactive agent runner might set `wait_cost = 20`.
- **`launch_order = "cheapest"`** ranks by the Spot price alone, as before; `"listed"` keeps the `instance_types` order.
- **A runner in one region with a fixed `instance_types` list** ranks by price, exactly as with `"cheapest"`: its options share the region's caches and distance, and spotexec doesn't second-guess the speed of types you listed.
- **Spot placement scores** (AWS's prediction of whether a request will be filled) aren't used: reading them needs `ec2:GetSpotPlacementScores`, which the spotexec policy doesn't grant. Capacity refusals already move a launch to the next option.

`spotexec prices <runner>` lists the options in launch order with the score (RUN COST) and what speed, a cold region (with the parts it pays: `boot`, `deps`, `image`; run it in a project to check that project's caches), distance and recent failures (FAILURES, with what failed and when beside it) added to each; `--json` has the same per option (`run_cost`, with `failures` and `failure_history`). `spotexec doctor` shows the first three, and each launch says why it went where it did: the `provision` event's message and the `instance_launched` and `instance_running` events' `placement` (its rank, score and factors, what was passed over, and what the cheapest option would have added).

## 5. Check your Spot vCPU quota

Spot capacity is limited by a per-region vCPU quota, *All Standard (A, C, D, H, I, M, R, T, Z) Spot Instance Requests* (`L-34B43A08`). New accounts often start very low, sometimes 5 vCPUs: enough for one 4-vCPU VM, but not one 8-vCPU VM. `spotexec setup` sizes the starter fleet to it (4-vCPU VMs when an 8-vCPU one doesn't fit) and offers to request an increase when not even one VM fits.

A fleet with `max_instances` needs at least `max_instances × vCPUs per instance`, summed over every fleet in the region; for example, 4 × `c7a.2xlarge` (8 vCPUs each) needs 32 vCPUs. A fleet without `max_instances` (the default, as in the starter config) has no ceiling of its own, so the quota is what limits how many of its VMs run at once. `spotexec doctor` shows the current value and what the runner may use; it fails only when not even one VM fits, and prints the increase command whenever the quota holds a fleet below its `max_instances`. To check or raise it:

```sh
aws service-quotas get-service-quota --profile spotexec --region us-east-2 \
  --service-code ec2 --quota-code L-34B43A08 --query Quota.Value
# Requesting an increase needs an admin session (it isn't in the spotexec policy):
aws service-quotas request-service-quota-increase --region us-east-2 \
  --service-code ec2 --quota-code L-34B43A08 --desired-value 64
```

The quota is shared by every machine using the account. Before each launch, spotexec checks the live usage and shares the quota between the machines that have VMs in the region ([details](scaling.md#several-machines-one-spot-quota)). `spotexec status -A` shows each region's quota against what's using it.

When the quota refuses a launch (`MaxSpotInstanceCountExceeded`), spotexec doesn't treat it as a capacity shortage and retry. Runs queue for the fleet's running VMs, and new launches wait until one of them ends or five minutes pass. With no VM running and no other runner in the pool, `sx` fails at once (exit 125) with the region, the vCPUs a launch needs and the increase command. It never falls back to on-demand unless you set `on_demand_fallback`.

Small increases are usually approved within minutes to hours. Two features use on-demand instances, which count against the separate on-demand quota (`L-1216C47A`):
- `on_demand_fallback`;
- the `image bake` builder.

## 6. Check the network

By default nothing needs setting up. spotexec launches into the region's **default VPC** and:
- requests a public IPv4 address per instance (AWS bills public IPv4 addresses by the hour);
- creates a `spotexec-ssh` security group allowing TCP 22 from `ssh_cidr`. The default is `0.0.0.0/0`, with key-only authentication (see [SSH exposure](#ssh-exposure) below).

Your machine must be able to make outbound SSH connections (TCP 22) to EC2 public addresses. Some corporate networks block this.

If the region has no default VPC, `spotexec setup` offers to create it. When that isn't possible (your organization blocks it, or you'd rather use your own network), set `subnet_id` to a **public** subnet: one whose route table sends `0.0.0.0/0` to an internet gateway. spotexec then creates its security group in that subnet's VPC, or uses `security_group_ids` if you set them. Those groups must allow inbound TCP 22 from your address, and outbound HTTP/HTTPS for packages, the S3 cache and ECR.

### SSH exposure

By default every runner VM accepts SSH on port 22 from anywhere (`ssh_cidr = "0.0.0.0/0"`). That is deliberate: laptops roam between networks and several machines can share a fleet, so a source-address allowlist would break routinely. What keeps it safe is how sshd is set up on every spotexec VM, whatever the AMI's defaults:
- only public-key logins (`AuthenticationMethods publickey`; passwords, keyboard-interactive, host-based and GSSAPI logins are off);
- only as the runner `user` (`AllowUsers`); root logins are refused (unless `user` is `root`, which then logs in with the key only);
- the key is the one spotexec generated on your machine, injected at launch; no EC2 key pair is attached.

These settings go in `/etc/ssh/sshd_config.d/00-spotexec.conf`, which sorts ahead of drop-ins like cloud-init's own; on an image whose `sshd_config` would still override it, they go at the top of `sshd_config` instead. A custom `ami` can still loosen sshd deliberately (a `Match` block, another `AllowUsers` entry): the VM then logs a warning to `/var/log/spotexec-bootstrap.log`.

To restrict the source as well (for example in an account with Security Hub control EC2.13, which flags port 22 open to the world):
- **A stable address:** set `ssh_cidr` to it, such as `ssh_cidr = "203.0.113.4/32"`. spotexec then manages a separate `spotexec-ssh-<hash>` group with that rule. Runs from anywhere else can't connect.
- **A VPN or office range:** set `ssh_cidr` to that range.
- **Your own rules:** create security groups yourself and set `security_group_ids`; spotexec then uses them as they are and creates none. They must allow inbound TCP 22 from wherever you run `sx`, and outbound HTTP/HTTPS.

A changed `ssh_cidr` or `security_group_ids` applies to VMs launched afterwards. Run `spotexec down` to replace the ones already running (or change their security groups in the console).

## 7. Configure the runner

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

The VMs run **Ubuntu 24.04** (login user `ubuntu`), resolved fresh from Canonical's public SSM parameter for the runner's architecture. `bootstrap_script` runs as root under `bash -e`, so a failing step fails the bootstrap (see [`BOOTSTRAP_FAILED`](troubleshooting.md)). Use it to install what your commands need, and keep both scripts idempotent.

## 8. Verify end to end

```sh
spotexec doctor aws2           # credentials, identity, AMI, network, quota, console region
spotexec up aws2               # boots one VM and waits until its bootstrap has finished
spotexec status                # the VM, its slots and its idle timer
cd ~/code/my-app && spotexec init   # spotexec only syncs Git repos or directories with .spotexec.toml
sx -r aws2 uname -a            # a first remote command
spotexec image bake aws2       # optional: snapshot the bootstrap for ~bare-boot cold starts
spotexec down aws2             # or simply let it idle out
```

If `up` fails, the error says why: a credential problem names the login command to run, a capacity error names the instance type, and a bootstrap failure prints the end of the bootstrap log. A VM that `up` launched but couldn't get ready (or that you interrupted with Ctrl-C) is terminated rather than left to idle out; after a bootstrap failure, runs stop launching new VMs of that runner for an hour, or until you fix the script and run `spotexec down aws2`. A VM that did finish booting is left running for other runs (it idles out as usual).

## What gets created in your account

| Resource | When | Cleanup |
| --- | --- | --- |
| IAM user `spotexec-<hostname>` (path `/spotexec/`) with an inline `spotexec` policy and one access key | `spotexec setup` | `spotexec setup --revoke <host>` for one machine ([revoking a machine](setup.md#revoking-a-machine)), or `spotexec teardown`. Then remove the `[spotexec]` profile from that machine's `~/.aws/credentials`. |
| The region's default VPC | `spotexec setup`, only if the region had none | It's AWS's standard network and costs nothing; keep it. |
| Spot instances tagged `spotexec-runner` / `spotexec-owner` | On demand | They terminate themselves; `spotexec down <runner>` (or `--all` for every fleet) terminates this machine's now, `spotexec down -A` every machine's. |
| On-demand `image bake` builders tagged `spotexec-bake-runner` / `spotexec-owner` | `spotexec image bake` | Terminated when the bake ends. One left behind (stopped, or older than four hours) is terminated by `spotexec down <runner>` or the pool manager; `spotexec down -A` terminates any machine's builders at once. |
| Security group `spotexec-ssh` (`spotexec-ssh-<hash>` for a custom `ssh_cidr`, one per VPC), tagged `spotexec-owner` | First launch without `security_group_ids` | `spotexec teardown` (below). |
| S3 bucket `spotexec-cache-<account>-<region>` | First run with `[deps]` (unless `cache_bucket = "off"`) | Objects expire after 14 days, through a lifecycle rule added beside any rules the bucket already has. The rule is best effort: a key whose policy predates reading those rules (`s3:GetLifecycleConfiguration`) leaves it unset until `spotexec setup` refreshes the policy. `spotexec teardown` deletes the bucket. |
| ECR repositories `spotexec/<repo-id>` | First Dockerfile build | Keeps the newest 3 builds per region, expiring older ones and their untagged entries; `spotexec teardown` deletes the repositories. |
| AMIs and snapshots | `spotexec image bake` | `spotexec image clear`, or `spotexec teardown`. |

To remove all of it at once, run `spotexec teardown` while signed in as an administrator. It searches every region enabled in the account (or those you pass with `--region`; for a GovCloud account, pass a `us-gov-*` region or give the admin profile one, so it signs in to that partition) and lists every instance, AMI, snapshot, security group, ECR repository, cache bucket and IAM user spotexec created, from any machine. It asks before deleting them (`--dry-run` only lists them; `--yes` skips the question). It then deactivates every machine's key, so nothing new is launched, terminates the VMs (including any launched in the meantime) and waits until they're gone before removing the rest. It reports anything it couldn't list or delete with a non-zero exit, so you can re-run it. A cache bucket with versioning turned on is emptied version by version, delete markers included; a version held by Object Lock retention or a legal hold keeps its bucket, and the error names that version. It only touches resources with spotexec's tags or exact names: a `cache_bucket` you named yourself, a security group you made, an untagged snapshot or an IAM user outside `/spotexec/` is left alone. So are the default VPC and the Spot service-linked role. Delete `~/.config/spotexec` and the `[spotexec]` profile yourself.

The SSH key is generated locally at `~/.config/spotexec/keys/id_ed25519` and injected through user-data, so no EC2 key pair is involved. Each instance gets its own `known_hosts` file, filled on first connect.

## Cost guardrails

The fleet shuts itself down without your laptop. One case is not covered: a VM whose kernel hangs before its timers fire. For a backstop that is fully independent of spotexec, add either of these to your account:
- an **AWS Budgets** alert;
- an **EventBridge** rule that terminates instances older than N hours that carry a `spotexec-runner` tag (fleet VMs) or a `spotexec-bake-runner` tag (`image bake` builders, which are on-demand).

`spotexec status` lists pending and running instances only. Stopped instances (whose volumes still bill) don't appear there; `spotexec down` terminates them along with the rest. `spotexec status -A` lists every spotexec instance in any state but terminated, including `image bake` builders, and `spotexec down -A` terminates them. Both search the regions (and profiles) your AWS runners use, say which, and fail if a region couldn't be listed.

## The life of a Spot VM

1. **Launch.** A one-time Spot request is made for the first option in launch order among the runner's instance types and zones: the cheapest Spot price, or with several regions or `vcpus` the lowest [expected cost of the run](#rank-by-the-cost-of-the-run) (see [Launch on the cheapest instance](#launch-on-the-cheapest-instance)), at a price capped by `max_price` (default: the on-demand price), with IMDSv2 required and a public IPv4 address. Capacity errors move on to the next-cheapest option; with `on_demand_fallback`, the last resort is on-demand.
2. **Arm.** The user-data script's first action installs the idle watchdog (a systemd timer) plus an independent `max_lifetime` backstop. Only then does it authorize the SSH key, map `localhost` to `::1` as well as `127.0.0.1`, and run your bootstrap scripts. `spotexec cloud-init <runner>` prints the exact script.
3. **Bootstrap.** Your scripts run under the lifecycle lock, so a long setup can't be mistaken for idleness. If one fails, the VM is marked failed: it refuses work and returns `BOOTSTRAP_FAILED` with the tail of `/var/log/spotexec-bootstrap.log`. The fleet then stops launching for an hour instead of burning money on a broken script.
4. **Serve.** Jobs acquire slots, and every session refreshes the activity clock.
5. **Interruption.** On a Spot interruption notice the VM drains: running jobs continue and new ones are refused. The CLI starts finding replacement capacity right away, in another zone or instance type if there is one (see [Launch on the cheapest instance](#launch-on-the-cheapest-instance)). A job that finishes within the 2 minutes is used normally. A job whose VM disappears first, or whose command is killed by the VM shutting down, is **replayed once** on the replacement: sync, deps, run and write-back. Nothing from the lost attempt was written locally. Rebalance recommendations are only reported, because EC2 often sends them the moment a VM boots, and draining on them makes a fleet churn.
6. **Exit.** After `idle_timeout` with no sessions, or immediately once a draining VM has no sessions, or at `max_lifetime`, the VM powers itself off, which terminates it.

> **Replays and side effects.** A replayed command may already have run to completion the first time. Commands with outside effects (deploys, publishing) should dedupe on `SPOTEXEC_RUN_ID`, or run with `--no-retry`.
