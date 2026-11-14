# Operations and troubleshooting

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
| `vCPU quota … has no room for another VM` | The account's Spot vCPU quota in that region is used up. Runs queue for VMs already running; with none, `sx` exits 125 and prints the `aws service-quotas request-service-quota-increase` command to run as an administrator. See [the quota section](aws.md#5-check-your-spot-vcpu-quota). |
| `InsufficientInstanceCapacity` | Add more `instance_types`, or try another region or availability zone. |
| Sync is slow or refused as too large | Run `spotexec sync-plan`. Exclude build outputs and large fixtures with `sync.exclude` or `.spotexecignore`. |
| `SYNC_CONFLICT` | You edited a file during the run. Compare it with the saved copy under `.git/spotexec/conflicts/<run-id>/`. |
| A test needs a local service | Prefix it with `SPOTEXEC_LOCAL=1` (agent hook), or run it with plain `pnpm …` instead of `sx`. |
| Expired SSO session | `aws sso login` (or `aws login`). spotexec reports this instead of falling back to other keys. |
