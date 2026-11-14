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

`spotexec setup` attaches this policy to the user it creates. To grant it yourself, attach it to the identity you signed in with; `spotexec setup --print-policy > spotexec-policy.json` saves it. The first three statements are required.

The other five are optional:
- S3, for the [dependency cache](environments.md#dependency-cache) (without it, set `cache_bucket = "off"` on the runner);
- the two ECR statements, for Dockerfile-built [containers](environments.md#project-containers);
- `servicequotas:GetServiceQuota`, for `doctor`'s quota check;
- the last statement, for [image baking](environments.md#baked-images).

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

## 4. Choose a region

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

## 5. Check your Spot vCPU quota

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

## 6. Check the network

By default nothing needs setting up. spotexec launches into the region's **default VPC** and:
- requests a public IPv4 address per instance (AWS bills public IPv4 addresses by the hour);
- creates a `spotexec-ssh` security group allowing TCP 22 from `ssh_cidr`. The default is `0.0.0.0/0`, with key-only authentication; narrow it to your address when it's stable, for example `ssh_cidr = "203.0.113.4/32"`.

Your machine must be able to make outbound SSH connections (TCP 22) to EC2 public addresses. Some corporate networks block this.

If the region has no default VPC, `spotexec setup` offers to create it. When that isn't possible (your organization blocks it, or you'd rather use your own network), set `subnet_id` to a **public** subnet: one whose route table sends `0.0.0.0/0` to an internet gateway. spotexec then creates its security group in that subnet's VPC, or uses `security_group_ids` if you set them. Those groups must allow inbound TCP 22 from your address, and outbound HTTP/HTTPS for packages, the S3 cache and ECR.

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
| IAM user `spotexec-<hostname>` (path `/spotexec/`) with an inline `spotexec` policy and one access key | `spotexec setup` | Delete the user in IAM; also remove the `[spotexec]` profile from `~/.aws/credentials`. |
| The region's default VPC | `spotexec setup`, only if the region had none | It's AWS's standard network and costs nothing; keep it. |
| Spot instances tagged `spotexec-runner` / `spotexec-owner` | On demand | They terminate themselves; `spotexec down` terminates them now. |
| Security group `spotexec-ssh` (`spotexec-ssh-<hash>` for a custom `ssh_cidr`, one per VPC) | First launch without `security_group_ids` | Delete by hand if you stop using spotexec. |
| S3 bucket `spotexec-cache-<account>-<region>` | First run with `[deps]` (unless `cache_bucket = "off"`) | Objects expire after 14 days. |
| ECR repositories `spotexec/<repo-id>` | First Dockerfile build | Keeps the newest 10 images. |
| AMIs and snapshots | `spotexec image bake` | `spotexec image clear`. |

The SSH key is generated locally at `~/.config/spotexec/keys/id_ed25519` and injected through user-data, so no EC2 key pair is involved. Each instance gets its own `known_hosts` file, filled on first connect.

## Cost guardrails

The fleet shuts itself down without your laptop. One case is not covered: a VM whose kernel hangs before its timers fire. For a backstop that is fully independent of spotexec, add either of these to your account:
- an **AWS Budgets** alert;
- an **EventBridge** rule that terminates `spotexec-runner`-tagged instances older than N hours.

`spotexec status` lists pending and running instances only. Stopped instances (whose volumes still bill) don't appear there; `spotexec down` terminates them along with the rest.

## The life of a Spot VM

1. **Launch.** A one-time Spot request is made at a price capped by `max_price` (default: the on-demand price), with IMDSv2 required and a public IPv4 address. Capacity errors move on to the next instance type; with `on_demand_fallback`, the last resort is on-demand.
2. **Arm.** The user-data script's first action installs the idle watchdog (a systemd timer) plus an independent `max_lifetime` backstop. Only then does it authorize the SSH key and run your bootstrap scripts. `spotexec cloud-init <runner>` prints the exact script.
3. **Bootstrap.** Your scripts run under the lifecycle lock, so a long setup can't be mistaken for idleness. If one fails, the VM is marked failed: it refuses work and returns `BOOTSTRAP_FAILED` with the tail of `/var/log/spotexec-bootstrap.log`. The fleet then stops launching for an hour instead of burning money on a broken script.
4. **Serve.** Jobs acquire slots, and every session refreshes the activity clock.
5. **Interruption.** On a Spot interruption notice the VM drains: running jobs continue and new ones are refused. The CLI starts finding replacement capacity right away. A job that finishes within the 2 minutes is used normally. A job whose VM disappears first is **replayed once** on the replacement: sync, deps, run and write-back. Nothing from the lost attempt was written locally. Rebalance recommendations are only reported, because EC2 often sends them the moment a VM boots, and draining on them makes a fleet churn.
6. **Exit.** After `idle_timeout` with no sessions, or immediately once a draining VM has no sessions, or at `max_lifetime`, the VM powers itself off, which terminates it.

> **Replays and side effects.** A replayed command may already have run to completion the first time. Commands with outside effects (deploys, publishing) should dedupe on `SPOTEXEC_RUN_ID`, or run with `--no-retry`.
