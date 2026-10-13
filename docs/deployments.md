# Deployments

This page describes what `depper deploy` does on the host, how it fails safely, and how to inspect and undo a deployment.

## Release model

Each deployment creates a new immutable release directory and switches a symlink to it. Nothing in an existing release is modified.

```
<base>/                          default: ~/.local/share/depper/<name>
  releases/
    release-20261012T154501.237Z-3fa9c2d1/  one directory per release
    release-20261011T091200.912Z-8b1e07aa/
  current  -> releases/release-20261012T154501.237Z-3fa9c2d1     relative symlink
  previous -> releases/release-20261011T091200.912Z-8b1e07aa     relative symlink
  .depper/                       private metadata, mode 700
    environment                  service environment (mode 600)
    environment.manifest         key names and salted fingerprints (mode 600)
    fingerprint.salt             random salt (mode 600)
    lock/                        present only while an operation runs
    transactions/                per-operation backups (mode 700)
    previous-transaction         pointer used by rollback
    tunnel.json, cloudflared.yml owned tunnel state (only with [tunnel])
```

- The `current` and `previous` links are relative, so a base directory can be moved or renamed as a whole.
- The unit runs with `WorkingDirectory=<base>/current` and reads its environment from `<base>/.depper/environment`.
- Release ids have the form `release-YYYYMMDDTHHMMSS.mmmZ-<8 hex digits>`: a UTC timestamp with millisecond resolution and a random suffix. Releases are ordered by the timestamp in the id, never by the random suffix. Ids made by older depper versions and by the Python `deployer` have a second-resolution timestamp (`release-YYYYMMDDTHHMMSSZ-<8 hex digits>`); they remain valid, including as the argument of `rollback --to`. Ids that tie at second resolution are ordered by `created_at` in the release metadata.
- Operations that do not create a release (`rollback`, `secrets push`) name their transaction directories `rollback-<timestamp>-<hex>` and `secrets-<timestamp>-<hex>` in the same style.
- Keep persistent data (databases, uploads) outside release directories. A rollback or pruning removes release directories and does not carry such data across.

### Release metadata

Each release contains `.depper-release.json`. It holds no secrets.

```json
{
  "id": "release-20261012T154501.237Z-3fa9c2d1",
  "created_at": "2026-10-12T15:45:01Z",
  "source": "worktree",
  "git_commit": "9f2c1d0e4a7b5c6d8e9f0a1b2c3d4e5f60718293",
  "git_ref": "main",
  "git_dirty": true,
  "depper_version": "v0.2.0",
  "deployed_by": "alice@laptop"
}
```

Container releases in image mode add two more fields, `image` and `image_digest`, as in the table below.

| Field | Meaning |
| --- | --- |
| `source` | `worktree` or `git`. |
| `git_commit` | Full commit hash of `HEAD` (worktree) or of the deployed ref (git). Empty when the directory is not a git repository. |
| `git_ref` | The deployed ref (git source) or the checked-out branch name (worktree). |
| `git_dirty` | `true` when uncommitted changes were uploaded; `null` outside worktree mode or outside a git repository. |
| `depper_version` | The version of the depper binary that created the release. |
| `deployed_by` | `user@hostname` of the machine that ran depper. |
| `image` | Image mode only (omitted otherwise): the configured `[container].image`. |
| `image_digest` | Image mode only (omitted otherwise): the repository digest of the pulled image (`repo@sha256:...`), recorded when the engine reports one. |

### Source: worktree or git

- `source = "worktree"` (default) uploads the current directory with rsync, including uncommitted and untracked files, minus the [excludes](configuration.md#deploy). It is what you see on disk. `.gitignore` is not applied.
- `source = "git"`, or `depper deploy --ref REF`, runs `git archive` for the commit into a temporary local directory and uploads that. Only committed content is shipped. If the working tree has uncommitted changes depper prints a note that they are not part of the release. The temporary directory is removed afterwards. Run from a subdirectory of a repository, the archive is limited to that subdirectory.

## The deploy transaction

`depper deploy` runs these steps while holding the per-service lock. Everything before step 8 leaves the live service untouched.

1. Load and validate `depper.toml` and the dotenv source locally. `--dry-run` stops here after printing the plan; it never connects to the host.
2. Probe the host: resolve the base path and create the private metadata directories. The probe stops the deploy when it finds a Python `deployer` layout or an interrupted `depper migrate` (both with a hint to run `depper migrate`), or when a depper layout and a legacy layout both exist (see [Migrating from deployer](migrating-from-deployer.md#interrupted-and-conflicting-layouts)). The base path, whether set in `[deploy].path` or derived from the remote `$HOME`, may contain only letters, digits, and `. _ @ + / -` and may not end with `/`; a remote `$HOME` with other characters needs `[deploy].path`.
3. Refuse to continue if `[tunnel]` was removed from a service that owns a tunnel, or if the container engine is missing.
4. Acquire the lock.
5. Compute the new environment and its manifest; create the release and transaction directories.
6. Upload the release with rsync, then upload the metadata file, the start script (process mode), the environment candidate files, and the unit candidate. Candidates are written to temporary names next to the live files.
7. Run `[deploy].setup` in the new release directory, and for containers build the image, or pull it and tag it as `depper/<name>:<release-id>` (see [Containers](containers.md)).
8. Activate: copy the live environment, manifest, unit, and the `current`/`previous` link targets into `transactions/<release-id>/`, move the candidates into place, repoint `current`, run `systemctl --user daemon-reload`, `enable`, and `restart`.
9. Wait for health. With `[health]`, depper polls every `interval` until the check passes or `start_period` elapses, and fails immediately if the unit enters the failed state or restarts. Without `[health]`, it waits three seconds and requires the unit to be active with zero restarts.
10. Commit: record the old `current` as `previous`, save the applied configuration of the new release under `transactions/<release-id>/applied/`, and prune old releases.
11. If `[tunnel]` is configured, create or verify the tunnel and its connector. A tunnel failure is reported as an error after the application has been deployed successfully; the application stays deployed.

The deploy prints the release id, source, endpoint, health result, a names-only summary of environment changes, and any pruned releases. `--json` prints the same as one JSON object with the keys `service`, `target`, `host`, `base`, `mode`, `release`, `metadata`, `endpoint_url`, `tunnel_url`, `health`, `env`, `pruned`, `notes`, `warnings`, and `dry_run`.

### Failure and automatic restore

- A failure before activation (upload, setup, build, a bad unit): the new release directory, its transaction, and any image built for it are deleted. The live service was never touched.
- A failure after activation began (the restart fails, or the service does not become healthy): depper restores the backed-up environment, manifest, unit, and links, reloads systemd, restarts the previous unit, and then deletes the failed release. The command exits with status 1 and an error that says the deployment was rolled back.
- If the restore itself fails, the error says so, names the transaction directory that was kept, and lists the recovery steps. The backup in `transactions/<release-id>/` is left in place for that purpose. The steps are:
  1. If depper reports that the lock is still held, run `depper unlock --force`, but only when no other depper operation is running.
  2. Run `depper rollback`. Without `--to` it restores the state saved before the interrupted operation (see [Interrupted operations](#interrupted-operations)). The error also names the release that was live before, which `depper rollback --to ID` switches to; when no release was live before, the bare `depper rollback` stops and disables the service.
- Ctrl-C and SIGTERM cancel the operation. The running `ssh` or `rsync` is given 2 seconds to finish on its own and is then killed (it runs in its own process group, so the terminal's Ctrl-C reaches only depper). Cleanup then runs on a fresh context: if activation had begun the previous state is restored, and the lock is released last. A deploy cancelled this way leaves its release directory on the host, because a setup command may still be running there, and marks it discarded (`discarded: true` in `depper releases --json`). The pruning step of the next successful deploy removes discarded releases. The command exits with status 1 and an error that says the deployment was interrupted (and rolled back, if activation had begun).

A first deployment has no previous state. If it fails after activation, the unit and files are removed again.

### Interrupted operations

If the machine running depper loses its connection or crashes mid-deploy, the host-side script may have stopped between steps. What remains depends on the point of interruption:

- Before activation, the lock and a half-uploaded release directory remain. Remove the lock with `depper unlock` once it is stale, and deploy again. If depper itself was killed rather than cancelled with Ctrl-C, the leftover release directory is an ordinary release that was never activated; it and its transaction are removed by pruning once newer releases push it out of the kept set. (A Ctrl-C leaves a release marked discarded, which the next successful deploy prunes at once.)
- After activation, the transaction backup `transactions/<id>/` still exists with an `interrupted` marker. Run `depper rollback` with no arguments: when it finds the backup of an interrupted deploy, rollback, or secrets push it restores the state saved before that operation (environment, manifest, unit, and links) instead of switching to the previous release, waits for health, and then removes the backup together with, for a deploy, the release and image it left behind. If several interrupted operations exist, the newest is restored. With `--json`, `restored_transaction` in the result names the transaction that was restored; it is empty for an ordinary rollback. Then run `depper status` to confirm.

If the service is down and you are unsure of the state, `depper status` and `depper releases` show what the host believes, and `depper logs` shows the journal.

## Pruning

After each successful deploy, depper keeps the newest `keep_releases` releases (default 5) plus the current and previous releases, and deletes the rest. Pruning also removes:

- the transaction backup of every pruned release. This bounds how long old environment files, which contain secret values, remain on the host;
- leftover transaction directories from finished operations (`rollback-*`, `secrets-*`);
- container images: in build and image mode, the `depper/<name>:<release-id>` tags of pruned releases, and any other image that carries the label `depper.service=<name>` for this exact service and whose release no longer exists. Images of other services are never touched.

Releases marked discarded (from an interrupted deploy) are removed on the next pruning regardless of `keep_releases`, and do not count toward it.

Pruning failures are reported as warnings; the deployment itself already succeeded.

## Listing releases

```sh
depper releases
```

```
release-20261012T154501.237Z-3fa9c2d1  9f2c1d0e4a7b main+dirty  by alice@laptop  (current)
release-20261011T091200.912Z-8b1e07aa  41c0be77d2a0 main  by alice@laptop  (previous)
release-20261009T120000.004Z-a07c5511  0d9e3f5a1c22 main  by alice@laptop
```

A trailing `[no saved configuration]` means the release directory exists but its applied environment and unit were not kept, so `rollback --to` cannot use it. `--json` prints `{"current", "previous", "releases": [{"id", "current", "previous", "rollback_available", "discarded", "metadata"}]}`. `discarded` is true for a release left behind by an interrupted deploy; the human listing does not mark it.

## Rollback

```sh
depper rollback                         # switch to the previous release
depper rollback --to release-20261009T120000.004Z-a07c5511
```

If an earlier deploy, rollback, or secrets push was interrupted and its automatic restore failed, a bare `depper rollback` restores the state saved before it instead; see [Interrupted operations](#interrupted-operations).

Rollback switches `current` to the target release and restores the environment and unit that release was deployed with (not the current environment), restarts the service, and waits for health. The release that was current becomes `previous`, so running `depper rollback` twice returns to where you started. If the restarted service is not healthy, rollback restores the state from before the rollback and exits with status 1.

`--to ID` works when the release directory exists and its applied configuration was kept (`rollback_available` in `depper releases --json`). Without `--to`, rollback needs a previous release. Rolling back to the current release is an error.

Rollback does not undo database migrations, uploaded files, or other external state. For container releases, see [Containers](containers.md#rollback).

An environment changed with `depper secrets push` is recorded for the current release, so a later rollback to a different release uses that release's own saved environment.

## Checking for drift

```sh
depper check
depper check --ref origin/main
```

`check` compares the commit recorded in the current release with `git rev-parse` of the local ref (default `HEAD`), compares the local environment with the host's manifest, and reports the unit state.

| State | Meaning |
| --- | --- |
| `up-to-date` | Deployed commit equals the local commit. A note is added if the release also contained uncommitted changes. |
| `outdated` | The commits differ. |
| `not-deployed` | No release is current. |
| `unknown` | Not a git repository, or the release has no recorded commit. |

The exit status is 0 when up to date, 3 for `outdated`, `not-deployed`, or secrets drift, and 1 for an error. `unknown` does not by itself cause exit 3. JSON keys: `service`, `target`, `state`, `reason`, `local_ref`, `local_commit`, `deployed`, `secrets`, `unit`, `drift`.

## Locking

Operations that change a deployment (`deploy`, `rollback`, `secrets push`, `tunnel`, `migrate`) hold a per-service lock, the directory `<base>/.depper/lock`, created atomically with `mkdir`. The directory contains `info` with `created_at`, `holder` (`user@host` of the machine running depper), `pid`, `command`, and a random ownership token. The token is never printed and is not part of any JSON output. Read-only commands (`status`, `health`, `releases`, `check`, `logs`) do not take the lock.

- A second operation on the same service fails immediately and names the holder.
- An operation releases only a lock that still carries its own token. If the lock was removed or taken over in the meantime, the release fails with an error and leaves the other holder's lock alone.
- A running operation keeps its lock fresh: depper refreshes it (touches the lock directory) every 5 minutes, and the remote scripts that run long commands (`setup`, container builds) refresh it from the host every minute. A lock is therefore stale when it has not been refreshed for 15 minutes, measured with the deploy host's clock, not when it was created 15 minutes ago.
- `depper unlock` removes a lock only if it is stale. Use `depper unlock --force` to remove a lock that was refreshed more recently, and only after confirming nothing is still running. Breaking a lock while a deploy runs can leave a release half-installed. `unlock` removes only the lock it inspected: if that lock changed hands between the inspection and the removal, the command fails and leaves the new lock in place.
- `depper unlock --json` prints `{"removed", "lock": {"held", "info": {"created_at", "holder", "pid", "command"}, "age_seconds", "stale"}}`. `age_seconds` is the time since the lock was last refreshed (or taken, if never refreshed), or -1 when the host's clock values cannot be read.
- `depper doctor` reports a held lock with the holder and "last refreshed X ago", as a warning when it is stale.

Locks protect concurrent depper runs on one service. They do not coordinate with other tools that edit the same directory.

## Other commands

- `depper restart` and `depper stop` run `systemctl --user restart|stop` on the unit.
- `depper logs [--lines N] [-f]` runs `journalctl --user-unit depper-<name>.service`.
- `depper status` is described in [Health checks and status](health.md#depper-status).
- A deployment is a short interruption: the unit restarts as part of activation. depper does not provide blue-green or rolling updates.
