# Development

## Requirements

- Go 1.24 or newer (`go.mod` declares `go 1.24`).
- `rsync` and a POSIX `sh` on the machine that runs the tests. The end-to-end tests use them to simulate a host.
- `git`, for the tests that exercise `git archive` sources.

Dependencies are deliberately few: `github.com/BurntSushi/toml` for configuration and `github.com/spf13/cobra` for the command line. Everything else is the standard library. depper drives the system `ssh` and `rsync` binaries with `os/exec` and does not link an SSH library, so `~/.ssh/config`, agents, and `ProxyJump` apply.

## Building

```sh
go build -o depper ./cmd/depper
./depper --version                  # "depper version dev"
go run ./cmd/depper --help
```

Release builds embed the version:

```sh
go build -trimpath -ldflags "-s -w -X github.com/frgmt0/depper/internal/version.Version=v0.2.0" -o depper ./cmd/depper
```

## Testing

From the repository root:

```sh
go build ./...
go vet ./...
go test ./...
go test -race -timeout 45m ./...
gofmt -l .                          # prints nothing when formatting is correct
```

The `test` workflow runs `go vet`, the gofmt check, and `go test -race -timeout 45m ./...` on Ubuntu and macOS for every push and pull request. The `internal/engine` tests run a fake host and take about a minute without `-race` and about 16 minutes with it on an M-series Mac, so the race run needs a timeout above Go's default of 10 minutes; use `-timeout 45m` for a local `-race` run as well.

### Tests never contact a real host

No `go test` test connects to a real machine, and none should. (The [end-to-end workflow](#end-to-end-tests) is separate: it runs in CI and targets the runner VM itself.) The suite uses fakes at two levels:

- An injectable executor. Everything that runs a local program (`ssh`, `rsync`, `git`) goes through the `transport.Executor` interface. `transport.Local` runs real processes, `transport.DryRun` prints commands, and `transporttest.Recorder` records calls and returns canned results, so command-line construction and parsing are tested without running anything.
- A fake host in a temporary directory, for end-to-end flows (`internal/engine`, `internal/tunnel`). The test binary re-executes itself as a fake `ssh` and `rsync` (selected with the `DEPPER_FAKE` environment variable through small wrapper scripts placed first on `PATH`). The fake `ssh` runs the received script with `sh -s` against a fake home directory and fake `systemctl`, `journalctl`, `docker`, `curl`, and `cloudflared`; `ssh -W` serves canned HTTP responses; the fake `rsync` copies with the local `rsync`. Deploy, rollback, secrets, lock, health, migration, and tunnel behaviour are exercised this way.

When you add behaviour that talks to a host, extend the fakes rather than testing against a machine. Write table-driven tests where inputs vary. Tests must not print secret values, and assertions on output should check that dotenv values never appear.

### End-to-end tests

`.github/workflows/e2e.yml` runs depper against a real host. The host is the GitHub-hosted `ubuntu-latest` runner itself, a full Ubuntu virtual machine with systemd and passwordless `sudo`: the job starts the runner's sshd, authorizes two freshly generated keys (an administrator key for `root` and a deploy key), and uses `127.0.0.1` as the deploy target. Nothing else is contacted except GitHub, the Ubuntu archive (apt), and docker.io (container images). The unit tests use fakes (see above); this workflow checks the scripts against real `sshd`, `useradd`, `loginctl`, `systemctl --user`, `ufw`, rsync, and podman. It exercises the `root` administrator path only; the `sudo` administrator path is covered by unit tests alone.

It runs on every push to `main` and to `feature/**` branches, and on demand. The job has a 30 minute timeout, and a newer run on the same ref cancels the older one. In order, the steps:

1. Build depper, then write the helpers: `dep` runs depper with a timeout, keeps its output under `$RUNNER_TEMP/e2e/logs`, and fails unless the exit code is the expected one; `check` asserts on JSON output with `jq -e`.
2. Start sshd (through `ssh.socket` on Ubuntu 24.04) and create the keys.
3. `depper host init` on the `examples/basic` project, then assertions on the deploy user, lingering, the user manager, `/etc/depper/host.json`, the written `depper.toml`, and the mode of `.depper/known_hosts`.
4. `doctor`, `deploy`, `health` (an `ssh -W` check), and `status`; a second deploy, `check`, and `rollback`.
5. `secrets diff` and `secrets push` with a test-only secret that the job generates; it checks that the running service has the value, by hash, and that the value never appears in depper's output.
6. `releases`, `logs`, a second `host init` that must change nothing, and `host status`.
7. `unlock` with leftover and stale locks (a depper process killed mid-deploy), and the raw `ssh -W` forward that health checks use.
8. podman in build mode (`examples/container`), image mode (digest pinning, nginx), and compose mode (`examples/compose` with `podman-compose`). Before the build-mode step the job removes the runner image's podman packages, so that `host init` installs them as it would on a fresh VPS.
9. `host init --harden-ssh --firewall 22`, then checks that password login is refused, root key login still works, ufw is active, the deploy user still deploys, and a repeat run changes nothing.
10. On failure, a diagnostics step prints the depper logs, the deploy user's journal and units, sshd's configuration, and the firewall status.

The runner image puts its own user's directories in `/etc/environment` (`XDG_RUNTIME_DIR=/run/user/1001`, `XDG_CONFIG_HOME=/home/runner/.config`). The "Start sshd" step saves those `XDG_` lines to `$RUNNER_TEMP/e2e/xdg-environment` and deletes them from `/etc/environment`, so the runner behaves like a fresh VPS and the deploy user's manager starts normally. Later, the "services ignore a foreign XDG environment" step gives the running deploy user manager the saved values with `systemctl --user set-environment` and checks that the main process of each depper service has the deploy user's `XDG_RUNTIME_DIR` and none of the foreign values. depper's own scripts ignore an `XDG_RUNTIME_DIR` or `XDG_*_HOME` that the current user does not own (`shell.SessionEnv`, covered by unit tests). The e2e test does not cover a user manager that starts with the foreign values, because depper does not control what the manager inherits. See [Preparing a host](hosts.md#another-users-xdg-variables-in-etcenvironment).

To run it by hand, open the repository's Actions tab, choose the `e2e` workflow, and use "Run workflow" for a branch, or run `gh workflow run e2e.yml --ref BRANCH`. GitHub lists a `workflow_dispatch` workflow only once the file is on the default branch. Do not run these steps on your own machine: they create users, install packages, change sshd, and enable a firewall. To investigate a failure, read the diagnostics step and the `*.out` and `*.err` logs it prints, and reproduce on a disposable virtual machine.

## Code layout

| Package | Responsibility |
| --- | --- |
| `cmd/depper` | `main`, calls `cli.Main`. |
| `internal/cli` | cobra commands, flags, exit-code mapping, JSON output. One file per command, registered from `init`. |
| `internal/config` | Parsing and validation of `depper.toml`, target overlays, defaults. |
| `internal/engine` | The lifecycle on the host: probe, lock, upload, deploy transaction, rollback, health, status, check, releases and pruning, secrets manifest, migration. |
| `internal/transport` | ssh and rsync invocation, option building, `ssh -W` dialer, executors. |
| `internal/shell` | POSIX quoting and script building for remote scripts. |
| `internal/unit` | Escaping for systemd unit and environment files. |
| `internal/dotenv` | The dotenv parser and source selection. |
| `internal/tunnel` | Cloudflare Tunnel management. |
| `internal/doctor` | Host readiness checks. |
| `internal/host` | `depper host init` and `host status`: host facts, plan, host key pinning, sshd hardening, the host record, and the `depper.toml` edit. |
| `internal/detect` | Project detection for `depper init`. File-based only; executes nothing. |
| `internal/ci` | GitHub Actions workflow generation. |
| `internal/exitcode` | Exit codes carried through errors. |
| `internal/version` | Build version. |

Conventions:

- Remote scripts are lists of lines; every interpolated value is validated and passed through the shared `shell.Quote`. Scripts are sent on ssh's standard input, never in `argv`.
- Secret values never go in `argv`, rsync, logs, or error messages. Errors name keys and line numbers only.
- Errors are wrapped with context and carry a message fit for the user; user errors do not panic.
- Exit codes: 0 success, 1 error, 2 unhealthy, 3 drift or outdated. Commands that already printed their result return a silent coded error.
- Stable JSON keys are part of the interface. Change them only with a changelog entry.

## Documentation

Every claim in these documents should match the code. When you change a default, a validation rule, a JSON field, or an exit code, update the relevant page in `docs/` and `CHANGELOG.md` in the same change. Use `depper <command> --help` output as the reference for flags.

## Releasing

Releases are cut from tags and built by GoReleaser.

1. In `CHANGELOG.md`, replace "Unreleased" in the version heading with the release date.
2. Make sure `go vet ./...`, `gofmt -l .`, and `go test -race -timeout 45m ./...` pass on the main branch.
3. Tag and push:

   ```sh
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

4. The `release` workflow (`.github/workflows/release.yml`, triggered by tags matching `v*`) runs GoReleaser with `release --clean`. It builds `./cmd/depper` with `CGO_ENABLED=0 -trimpath` for linux and darwin on amd64 and arm64, injecting the version with `-X github.com/frgmt0/depper/internal/version.Version={{.Version}}`, and publishes a GitHub release.
5. Assets are `depper_<version>_<os>_<arch>.tar.gz` for each platform and `checksums.txt` (SHA-256). `{{.Version}}` has no leading `v`, so tag `v0.2.0` produces `depper_0.2.0_linux_amd64.tar.gz`.

The archive name template in `.goreleaser.yaml` must stay in sync with `ci.ArchiveName` in `internal/ci`, because the workflow generated by `depper ci init` downloads `depper_<version>_linux_amd64.tar.gz` from the release matching the depper version that generated it.

To test the release configuration locally without publishing: `goreleaser release --snapshot --clean` (requires GoReleaser v2).

### The install script

`install.sh` at the repository root is the installer documented in the README. It downloads `depper_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from the GitHub release and expects `depper` at the root of the archive. It must stay in sync with the `archives` section of `.goreleaser.yaml`: if the archive `name_template`, the supported `goos`/`goarch` values, the checksum file name, or the archive layout change, update `install.sh` (and `ci.ArchiveName`) in the same change.

Check it with:

```sh
sh -n install.sh
dash -n install.sh
shellcheck -s sh install.sh
```

Testing hook: `DEPPER_DOWNLOAD_BASE` replaces `https://github.com/frgmt0/depper/releases/download`. It must start with `https://` or `file://`; anything else is rejected. The script requests `$DEPPER_DOWNLOAD_BASE/v<version>/<archive>` and `.../checksums.txt`, so a local test can build a tarball and `checksums.txt` into `rel/v9.9.9/` and run:

```sh
DEPPER_VERSION=9.9.9 DEPPER_DOWNLOAD_BASE=file://$PWD/rel DEPPER_INSTALL_DIR=$PWD/inst sh install.sh
```

Always set `DEPPER_INSTALL_DIR` to a scratch directory when testing. The hook is not documented for users. The archive is still verified against the `checksums.txt` served from the same base, so the hook gives no protection against a hostile base URL, which is why users are not pointed at it.
