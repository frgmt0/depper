# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] - Unreleased

The tool is renamed from `deployer` to `depper` and rewritten in Go as a single binary. Local requirements are `ssh` and `rsync`; Python is no longer needed on either side.

### Added

- Single static binary, published for Linux and macOS (amd64 and arm64) with SHA-256 checksums through GoReleaser.
- Container services: `[container]` with docker or podman in `build`, `image`, or `compose` mode, including ports, volumes, extra run arguments, and a command override (`ports`, `volumes`, `args`, and `command` are rejected with `compose`). In build and image mode each release runs its own image tag, `depper/<name>:<release-id>`: image mode tags the pulled image, so a release keeps running the exact image it was deployed with, and records `image` and `image_digest` in the release metadata. Build-mode images carry the label `depper.service=<name>`, and pruning removes only images of its own service. The lowercased service name must be a valid image name.
- `[endpoint]` to record where a service is reachable without a tunnel.
- `[health]` with URL or command checks, `expect_status`, `timeout`, `start_period`, and `interval`. URL checks run from the host's network view over `ssh -W` (with `ControlPath=none` and a `LocalCommand` ready marker), with a remote `curl` fallback when forwarding is refused. `timeout` covers only the HTTP exchange; the SSH connection gets `max(15 s, timeout)`. A health command that exits with status 255 is reported as unhealthy, not as an ssh failure.
- `depper health` with `--watch` and `--count`, and `depper status`, both with stable `--json` output. Exit status 2 for an unhealthy service.
- Targets: `[targets.<name>]` overlays selected with `--target` or `DEPPER_TARGET`.
- `source = "git"` and `depper deploy --ref REF` to ship committed content with `git archive`.
- Release metadata (`.depper-release.json`), `depper releases`, pruning to `keep_releases` (default 5) including transaction backups and container images, and `depper rollback --to`.
- Release ids of the form `release-YYYYMMDDTHHMMSS.mmmZ-<hex>` (millisecond timestamp). Ordering uses the timestamp and never the random suffix. Older second-resolution ids remain valid, including for `rollback --to`.
- `depper check`: compares the deployed commit and secrets with the local repository; exit status 3 when the deployment is outdated, nothing is deployed, or secrets drift. `depper status` exits 0 whenever it could read the host, even if the service is unhealthy.
- Secrets management: `--env-stdin`, `depper secrets list`, `diff`, and `push`, backed by a per-deployment salt and HMAC-SHA256 fingerprints in an environment manifest. Values are never printed or read back from the host.
- `depper doctor` to check SSH, rsync, `sha256sum`, the systemd user manager, linger, disk space, the container engine, `cloudflared`, locks, and layout.
- `depper ci init` to generate a GitHub Actions workflow with per-target concurrency, a checksum-verified depper install, and secrets passed through `DEPPER_SSH_KEY`, `DEPPER_KNOWN_HOSTS`, and `DEPPER_ENV`. `DEPPER_SSH_KEY_FILE` and `DEPPER_KNOWN_HOSTS_FILE` override `identity_file` and `known_hosts`.
- A holder-aware lock: its `info` file carries a random ownership token (not shown in JSON output), an operation releases only its own lock, and `depper unlock` removes only the lock it inspected and fails if the lock changed hands. A lock is stale when it has not been refreshed for 15 minutes by the host's clock; running operations refresh it, and `depper unlock` removes a more recent lock only with `--force`. `age_seconds` in `unlock --json` is the time since the last refresh, and `depper doctor` reports "last refreshed X ago".
- `depper migrate` to convert deployments made by the Python `deployer`, with `--dry-run`. For default paths it leaves the relative compatibility symlink `~/.local/share/deployer/<name> -> ../depper/<name>`. It is resumable: rerun `depper migrate` after an interruption (the dry run says "resume the interrupted migration"). If it fails early, while the legacy metadata and unit still exist, it restarts the legacy units. Other commands report an interrupted migration, and `depper doctor` shows layout `migrating`. When a depper layout and a legacy layout both exist, every command that changes the deployment refuses and read-only commands warn.
- SSH options `user`, `port`, `identity_file`, and `known_hosts` (host key pinning with strict checking); IPv6 hosts.
- Automatic restore recovery guidance: after a failed restore the error lists the recovery steps, and `depper rollback` with no arguments restores the pre-operation state of the newest interrupted deploy, rollback, or secrets push, reported as `restored_transaction` in `rollback --json`.
- Ctrl-C and SIGTERM handling: the running `ssh` or `rsync` gets 2 seconds before it is killed, then cleanup runs and the lock is released. A deploy interrupted this way leaves its release marked discarded (`discarded: true` in `releases --json`), and the next successful deploy prunes it.
- `--json` output for every command that reports a result, with human messages moved to stderr.
- Documentation under `docs/` and Go-era examples under `examples/`.

### Changed

- Command, files, and host names: `depper`, `depper.toml` (`deploy.toml` is still read), `~/.local/share/depper/<name>`, `.depper/`, `depper-<name>.service`, and `depper-tunnel@<name>.service`.
- `current` and `previous` links are relative, so a deployment directory can be relocated.
- Old releases are pruned after a successful deploy; the Python tool kept all of them.
- The remote base path (`[deploy].path`, or the default under the remote `$HOME`) may contain only letters, digits, and `. _ @ + / -` (`^/[A-Za-z0-9._@+/-]+$`). A remote `$HOME` with other characters needs `[deploy].path`. `identity_file`, `known_hosts`, `DEPPER_SSH_KEY_FILE`, and `DEPPER_KNOWN_HOSTS_FILE` may not contain whitespace.
- Remote scripts run with `ssh -T` and are sent wrapped in one group, so the remote shell parses each script in full before running any of it.
- `[tunnel]` cannot be overridden per target.
- A deployment's transaction is recorded per release, and a release's applied environment and unit are retained so `rollback --to` restores exactly what that release ran.
- Container modes have no `container.env` file. In build and image mode the values come from the unit's `EnvironmentFile` and are passed with `--env KEY`; the key list is kept in the reserved variable `DEPPER_CONTAINER_ENV_ARGS`. Multi-line and quoted values are allowed; a value over 60 KiB is rejected, and the error names the key only. In compose mode every compose command runs with `--env-file /dev/null`, so a `.env` file in the repository or release is ignored, and `${VAR}` comes from depper's environment: the unit's `EnvironmentFile` at runtime and exported values during setup. Existing container deployments must run `depper deploy` once before `depper secrets push`.

### Security

- Secret values are never placed in command lines, rsync transfers, or output; uploads are atomic and private (modes 600 and 700), and symlinked metadata paths are refused.
- Old environment values are removed with the release that retained them when it is pruned.

## [0.1.0] - not tagged

Initial release of the Python tool `deployer`.

### Added

- `deploy.toml` configuration with `[service]`, `[deploy]`, `[env]`, `[secrets]`, and `[tunnel]`.
- Deployment of a repository to a Linux host over SSH and rsync as a systemd user service, with release directories, `current` and `previous` links, optional setup commands, and a health command.
- Automatic restore of the previous release when a deployment fails to start.
- `status`, `logs`, `restart`, `stop`, `rollback`, and `init` commands.
- Dotenv secrets (`[secrets].file` and `--env FILE`) stored in a private environment file on the host and never uploaded with the release.
- Optional Cloudflare Tunnel support with an owned tunnel per service and `tunnel`, `tunnel-status`, `tunnel-logs`, and `tunnel-stop` commands.
- Per-service deployment lock.

[0.2.0]: https://github.com/frgmt0/depper/releases/tag/v0.2.0
[0.1.0]: https://github.com/frgmt0/depper/commits/main
