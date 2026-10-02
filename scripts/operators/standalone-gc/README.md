# Configurable standalone worktree GC

This Linux companion sweep handles agent-created Git worktrees outside Multica's
registered task roots. It is separate from native daemon GC. All discovery paths,
project identity, card prefix, daemon-config path, lock path, and remote/base branch
are operator configuration, with no account-specific defaults or credentials.

Copy `config.example.json` to an operator-owned config and replace its synthetic
project UUID, example prefix and paths. `roots` scans direct child directories only:

- Every root accepts names containing `wt` and exactly one configured prefix/number,
  in either order (with prefix OPS, `ops12-wt` or `wt-ops-12`).
- Roots explicitly marked `allow_number_only: true` also accept exact `wt-<number>`.
- Linked roots and linked candidates are excluded. Managed task trees are not
  recursively scanned.

The sweep reads the workspace retention delay from `gc_ttl` in `daemon_config` on
every run. It removes only registered Git worktrees with a matching project/card
number, a done/cancelled card older than that delay, no detected active process, no
local modifications or non-ignored untracked files, and no commits missing from
the configured base according to `git cherry` after fetching. Card state, process
activity and cleanliness are rechecked before `git worktree remove` without force.
Unknown/failed checks retain the checkout and print a JSON reason.

Unsaved work is kept, rather than copied to salvage by this companion. Multi-commit
squash merges may conservatively remain. Process detection uses Linux `/proc` and
is best effort: this companion has no scheduler reservation, so a run can start
after the final check. Prefer native managed task roots whenever possible.

No change is made to native daemon defaults. An operator can select `gc_artifact_ttl`
`1h` and `gc_ttl` `30m` in the existing daemon config without replacing its other
settings. Native settings take effect after daemon startup. This companion's timer
is independently every 15 minutes; it does not read `gc_interval`.

## Verify

```sh
python3 -m unittest discover -s . -p test_sweep.py -v
python3 sweep.py --config /path/to/operator-config.json
```

The command defaults to dry-run; `--apply` is required for deletion. Dry-run fetches
remote refs to check preservation, but does not delete worktrees. Tests use local
Git fixtures and injected card responses, never a real account or agent CLI.

## Install on Linux

Run as the intended service account with a configured Multica CLI profile that can
read the intended project. Install the script and templated units as root:

```sh
sudo install -m 0755 sweep.py /usr/local/bin/multica-standalone-gc.py
sudo install -m 0644 multica-standalone-gc@.service multica-standalone-gc@.timer /etc/systemd/system/
sudo install -d -m 0755 /etc/multica
```

Save the reviewed operator configuration as `/etc/multica/standalone-gc-ACCOUNT.json`;
substitute the actual Unix account for ACCOUNT. The file should be root-owned and
readable by that account, and the configured lock directory must already exist and
be writable by it. Keep credentials solely in the existing CLI profile.

First run a dry-run under that account. After approving its results:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now multica-standalone-gc@ACCOUNT.timer
sudo systemctl start multica-standalone-gc@ACCOUNT.service
sudo systemctl show multica-standalone-gc@ACCOUNT.service -p Result -p ExecMainStatus
sudo journalctl -u multica-standalone-gc@ACCOUNT.service -n 20 --no-pager
```

To stop recurring deletion without removing any worktree:

```sh
sudo systemctl disable --now multica-standalone-gc@ACCOUNT.timer
```
