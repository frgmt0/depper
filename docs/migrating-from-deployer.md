# Migrating from deployer

depper is a rewrite of the Python tool `deployer` as a single Go binary. The configuration format carries over, but the names of files, directories, and systemd units on the host changed. Existing deployments keep running until you convert them with `depper migrate`.

## What changed

| | `deployer` (Python) | `depper` |
| --- | --- | --- |
| Command | `deployer` | `depper` |
| Config file | `deploy.toml` | `depper.toml`. `deploy.toml` is still read when `depper.toml` is absent; if both exist, depper stops with an error. |
| Default remote directory | `~/.local/share/deployer/<name>` | `~/.local/share/depper/<name>` |
| Metadata directory | `<base>/.deployer/` | `<base>/.depper/` |
| Start script in a release | `.deployer-start.sh` | `.depper-start.sh` |
| App unit | `deployer-<name>.service` | `depper-<name>.service` |
| Tunnel unit | `deployer-tunnel@<name>.service` | `depper-tunnel@<name>.service` |
| `current` and `previous` links | absolute | relative (`releases/<id>`) |
| Error prefix | `deployer:` | `depper:` |
| Requirements on the host | Python 3 | None from depper; POSIX sh, coreutils, rsync, systemd |
| Requirements locally | Python 3.11+ | A single binary; `ssh` and `rsync` |

A custom `[deploy].path` is not moved by migration; only its `.deployer` metadata directory is renamed.

Beyond renaming, depper adds: `[container]`, `[endpoint]`, `[health]` with URL checks and start periods, `[targets.<name>]`, git sources and `--ref`, release metadata and pruning, `releases`, `check`, `secrets list|diff|push`, `doctor`, `unlock`, `ci init`, and `--json` output. Notable behaviour differences:

- Old releases are pruned after a successful deploy. The Python tool kept every release. The default is `keep_releases = 5`.
- `[deploy].path` must match `^/[A-Za-z0-9._@+/-]+$`.
- Secrets are stored with a salted-fingerprint manifest so drift can be detected. See [Secrets](secrets.md).
- A deployment without `[health]` still waits three seconds and requires an active unit with no restarts, as before.

The Cloudflare tunnel feature behaves the same way. A migrated tunnel keeps its Cloudflare-side name `deployer-<name>` and UUID; depper does not rename or recreate it.

## Before you migrate

1. Install depper ([README](../README.md#install)).
2. In the repository, rename `deploy.toml` to `depper.toml` (optional but recommended). Either name works; do not keep both.
3. Make sure no Python `deployer` operation is running for this service.
4. Note the current state (`deployer status`). Migration keeps all releases, but there is no automatic undo (see the end of this page).

## Migrate

Look at the plan first. `--dry-run` inspects the host read-only and prints what would happen:

```sh
depper migrate --dry-run
```

```
DRY RUN: migrate api from /home/deploy/.local/share/deployer/api to /home/deploy/.local/share/depper/api:
  - stop and disable deployer-api.service
  - move /home/deploy/.local/share/deployer/api to /home/deploy/.local/share/depper/api
  - link /home/deploy/.local/share/deployer/api -> ../depper/api (relative), so absolute paths recorded inside existing releases keep working (for example the interpreter paths in a Python virtualenv's scripts)
  - rename /home/deploy/.local/share/depper/api/.deployer to .depper
  - point current at releases/release-20261011T091200Z-8b1e07aa (relative)
  ...
```

Then run it:

```sh
depper migrate
```

What it does, under the deployment lock of the metadata directory (`.deployer/lock`, which moves with the directory; it is released at the end only if it still carries migrate's own token):

1. Records the facts it needs to put things back (the unit's `Restart=` policy, whether the app and connector units were active and enabled) in a marker file inside the metadata directory, `migration-in-progress`, before it changes anything.
2. Stops and disables the legacy app unit (and the tunnel unit, if there is one).
3. Moves a default-path base directory from `~/.local/share/deployer/<name>` to `~/.local/share/depper/<name>` and leaves the compatibility symlink described below at the old location. A custom `[deploy].path` stays where it is.
4. Renames `.deployer` to `.depper`.
5. Rewrites `current` and `previous` as relative links.
6. Rewrites the stored `previous-transaction` pointer, the release links in each transaction backup, and the unit files backed up in them, so `depper rollback` still works for the releases that existed before migration.
7. Adds `.depper-start.sh` next to `.deployer-start.sh` in existing releases.
8. Installs `depper-<name>.service`, preserving the unit's `Restart=` policy.
9. If the service owns a tunnel: regenerates `cloudflared.yml` with the new paths, installs `depper-tunnel@<name>.service`, and keeps the tunnel name and UUID recorded in `tunnel.json` unchanged.
10. Removes the legacy unit files, reloads systemd, and starts the app (and the connector, if it was enabled).
11. Waits for the health check, as a deploy does.

`--json` prints the result as `{"service", "status", "legacy_base", "base", "plan", "health"}` where `status` is `migrated`, `planned`, or `already-migrated`.

Migration refuses to continue, with an explanatory error, when:

- both a `.depper` and a `.deployer` layout exist for the service;
- a lock exists in the metadata directory (`lock`), meaning a Python or depper operation is running or left a lock;
- `depper-<name>.service` or `depper-tunnel@<name>.service` already exists (unless the run resumes an interrupted migration);
- the destination directory already exists;
- `.deployer` is a symlink, or `tunnel.json` is invalid.

Any other depper command that finds only a legacy layout fails and tells you to run `depper migrate`. `depper doctor` reports it as a warning (layout `legacy`).

If a step fails while the Python deployer's metadata directory (`.deployer`) and its unit file still exist, depper restarts the legacy units that were active before (and re-enables those that were enabled), so the service keeps running as before, and then reports that the migration failed and can be run again. Once the metadata has been renamed and the legacy unit file removed, there is nothing to restart, and the next run finishes the migration instead.

### Compatibility symlink

Python virtual environments and other artifacts built inside releases often contain absolute paths such as `/home/deploy/.local/share/deployer/api/releases/<id>/.venv`. After the base directory moves, those paths would dangle. For default-path deployments, where the base directory moves, migration leaves a relative symlink at the old location:

```
~/.local/share/deployer/<name> -> ../depper/<name>
```

so existing releases keep working unchanged. The link is relative, so it keeps working if the home directory is relocated. If something other than this link already exists at the old path, migration prints a warning and does not create it. depper treats the link as part of the depper layout, not as a legacy deployment. New releases are built under the new path by your `setup` commands. Once every release that was created before migration has been pruned, you can remove the symlink:

```sh
rm ~/.local/share/deployer/<name>      # removes the link only
```

### Interrupted and conflicting layouts

Migration can be resumed. If it was interrupted (the connection dropped, the host rebooted), run `depper migrate` again; every step is skipped when it is already done, and the facts recorded in the `migration-in-progress` marker (restart policy, which units were active and enabled) are reused, so the end state is the same as an uninterrupted run. The marker is removed as the last step. `depper migrate --dry-run` on such a host starts its plan with:

```
  - resume the interrupted migration (marker /home/deploy/.local/share/depper/api/.depper/migration-in-progress); every step below is skipped when already done
```

While the marker exists, every other depper command that changes or reads the deployment stops with "a `depper migrate` of <name> was interrupted; run `depper migrate` again to finish it", and `depper doctor` reports layout `migrating` as a warning.

If both a depper layout (`<base>/.depper`) and a legacy layout (a `.deployer` directory, or a legacy unit file) exist for the same service, depper cannot tell which one is live. Every command that changes the deployment (`deploy`, `rollback`, `secrets push`, `restart`, `stop`, `unlock`, `tunnel`, `tunnel-stop`) refuses with an error that names both. Read-only commands (`status`, `health`, `releases`, `check`, `logs`, `secrets list`, `secrets diff`, `tunnel-status`, `tunnel-logs`) print a warning and continue. `depper migrate` refuses as well, and `depper doctor` reports layout `both` as a warning. Remove the layout you no longer need by hand; `depper migrate --dry-run` prints details.

## Verify

```sh
depper status
depper releases
depper doctor
depper health
```

`status` should show `depper-<name>.service` active with the same current release as before. On the host:

```sh
systemctl --user list-units 'depper-*' 'deployer-*'      # only depper-* units remain
ls -l ~/.local/share/deployer/ ~/.local/share/depper/    # the compatibility link and the new base
```

Notes about the migrated state:

- The environment file is carried over unchanged, but there is no secrets manifest yet. `depper secrets list` reports that none exists, and `depper secrets diff` reports `unknown`, until the next `depper deploy` or `depper secrets push` writes one.
- `depper rollback` works for the previous release and `depper rollback --to` for the release that was current at migration time. Older releases have no saved configuration (`[no saved configuration]` in `depper releases`) until they are redeployed.
- Old releases are pruned at the next successful deploy according to `keep_releases`.

After you are satisfied, uninstall the Python tool (`pipx uninstall deployer`, or remove its virtual environment) and rename `deploy.toml` to `depper.toml` in each repository if you have not already.

## Rolling back a migration

There is no reverse command. Before migrating a service you cannot afford to lose, copy the base directory and the unit file on the host. To revert by hand: stop `depper-<name>.service`, move the base directory back to `~/.local/share/deployer/<name>`, rename `.depper` to `.deployer`, restore the unit file as `deployer-<name>.service`, run `systemctl --user daemon-reload`, and start it. The Python tool needs absolute `current` links, which migration replaced with relative ones; recreate them with `ln -sfn`.
