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

No test connects to a real machine, and none should. The suite uses fakes at two levels:

- An injectable executor. Everything that runs a local program (`ssh`, `rsync`, `git`) goes through the `transport.Executor` interface. `transport.Local` runs real processes, `transport.DryRun` prints commands, and `transporttest.Recorder` records calls and returns canned results, so command-line construction and parsing are tested without running anything.
- A fake host in a temporary directory, for end-to-end flows (`internal/engine`, `internal/tunnel`). The test binary re-executes itself as a fake `ssh` and `rsync` (selected with the `DEPPER_FAKE` environment variable through small wrapper scripts placed first on `PATH`). The fake `ssh` runs the received script with `sh -s` against a fake home directory and fake `systemctl`, `journalctl`, `docker`, `curl`, and `cloudflared`; `ssh -W` serves canned HTTP responses; the fake `rsync` copies with the local `rsync`. Deploy, rollback, secrets, lock, health, migration, and tunnel behaviour are exercised this way.

When you add behaviour that talks to a host, extend the fakes rather than testing against a machine. Write table-driven tests where inputs vary. Tests must not print secret values, and assertions on output should check that dotenv values never appear.

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
