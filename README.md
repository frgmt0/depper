# depper

depper deploys a repository to a Linux host over SSH and runs it as a systemd user service. It copies the working tree (or a git ref) into a new release directory with rsync, installs a systemd user unit, restarts the service, waits for a health check, and restores the previous release automatically if the deployment does not become healthy. A deployment is described by a single `depper.toml` in the repository. Nothing is installed on the host beyond what the service itself needs; depper is a single local binary that drives the system `ssh` and `rsync`.

depper replaces the Python tool `deployer`. See [Migrating from deployer](docs/migrating-from-deployer.md) for existing deployments.

## What depper manages

- Release directories under `~/.local/share/depper/<name>` (or `[deploy].path`), with `current` and `previous` symlinks, release metadata, and pruning of old releases.
- One systemd user unit per service, `depper-<name>.service`, for either a process command or a docker/podman container (build, prebuilt image, or compose file).
- The service environment: plain variables from `[env]` and secrets from a local dotenv file, stored on the host in a private file with a salted-fingerprint manifest for drift detection.
- Health checks (HTTP URL or command), deployment transactions, automatic restore, rollback, and a per-service lock.
- Optional: a dedicated Cloudflare Tunnel per service, and a generated GitHub Actions workflow.

## What depper does not do

- Deploying does not provision hosts, install packages, open firewall ports, or enable lingering. The separate, explicit command `depper host init` does that once, as an administrator, and nothing else uses root. See [Preparing a host](docs/hosts.md).
- It does not provide zero-downtime deployments. The unit is restarted on every deployment, so there is a short interruption.
- It does not roll back databases, uploaded files, or any state outside the release directory.
- It does not manage system-wide units, Windows services, macOS launchd jobs, or Kubernetes.
- It is not a secret vault. Secrets are protected by file permissions on the host account. See [Secrets](docs/secrets.md).
- It does not run on a host without systemd, and it does not use a remote agent or daemon.

## Requirements

Local machine (where you run `depper`): `ssh`, `rsync`, and `git` (git is optional unless you deploy a ref or use `source = "git"`); `depper host init` also needs `ssh-keyscan`, which ships with the OpenSSH client. Linux or macOS binaries are published.

Deploy host: Linux with a systemd user manager, a POSIX `sh`, GNU-compatible coreutils (including `sha256sum`, `mktemp`, `base64`, and `mv -T`), and `rsync`. `curl` is optional and only used as a fallback for URL health checks. A container engine (docker or podman) is needed only when `[container]` is used, and `cloudflared` only when `[tunnel]` is used. No Python is required by depper; your application may need its own runtime.

The remote base directory (`[deploy].path`, or the default under the remote `$HOME`) may contain only letters, digits, and `. _ @ + / -`. A remote `$HOME` with other characters needs `[deploy].path`. Local `identity_file` and `known_hosts` paths, and the CI variables `DEPPER_SSH_KEY_FILE` and `DEPPER_KNOWN_HOSTS_FILE`, may not contain whitespace.

Authentication is non-interactive (`ssh -o BatchMode=yes`): use SSH keys or an agent. Password prompts are not supported.

## Install

depper is a single binary. These methods install it for your user or system-wide; none of them changes anything else on the machine. Supported platforms: Linux and macOS, amd64 and arm64.

### Install script

```sh
curl -fsSL https://raw.githubusercontent.com/frgmt0/depper/main/install.sh | sh
```

If you prefer not to pipe a script to `sh`, download it, read it, then run it:

```sh
curl -fsSLO https://raw.githubusercontent.com/frgmt0/depper/main/install.sh
less install.sh
sh install.sh
```

The script detects your OS and architecture, downloads the release archive and `checksums.txt`, verifies the SHA-256 checksum (aborting on a mismatch), and installs `depper` with mode 0755. It never uses `sudo`. It prints the installed version and, if the install directory is not on your `PATH`, the line to add.

| Variable | Meaning |
| --- | --- |
| `DEPPER_VERSION` | Version to install, `0.2.0` or `v0.2.0`. Default: the latest release. |
| `DEPPER_INSTALL_DIR` | Directory to install into. Default: `/usr/local/bin` if it is writable, otherwise `$HOME/.local/bin` (created if missing). |

```sh
DEPPER_VERSION=0.2.0 DEPPER_INSTALL_DIR="$HOME/bin" sh install.sh
```

To install into `/usr/local/bin` when your user cannot write to it, run the script as root yourself:

```sh
curl -fsSL https://raw.githubusercontent.com/frgmt0/depper/main/install.sh | sudo DEPPER_INSTALL_DIR=/usr/local/bin sh
```

If the install directory is not on your `PATH`, add it. For bash or zsh, put this in `~/.bashrc` or `~/.zshrc`:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

For fish:

```fish
fish_add_path $HOME/.local/bin
```

### Release binary

Archives for Linux and macOS (amd64 and arm64) are attached to each [GitHub release](https://github.com/frgmt0/depper/releases) together with `checksums.txt` (SHA-256). Replace `VERSION`, `OS` (`linux` or `darwin`), and `ARCH` (`amd64` or `arm64`):

```sh
VERSION=0.2.0 OS=linux ARCH=amd64
base="https://github.com/frgmt0/depper/releases/download/v${VERSION}"
archive="depper_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "${base}/${archive}"
curl -fsSLO "${base}/checksums.txt"
grep " ${archive}\$" checksums.txt | sha256sum -c -      # on macOS: ... | shasum -a 256 -c -
tar -xzf "${archive}" depper
install -m 0755 depper /usr/local/bin/depper
depper --version
```

The checksum file lives in the same release as the archive, so it detects corruption and mismatched downloads but does not prove authorship. Compare it against the release page if that matters to you.

### go install

```sh
go install github.com/frgmt0/depper/cmd/depper@latest
```

This requires Go 1.24 or newer. The binary is placed in `$(go env GOPATH)/bin` (or `$GOBIN`), which must be on your `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Add that line to `~/.bashrc` or `~/.zshrc` to keep it. A binary built this way reports its version as `dev`.

### From source

```sh
git clone https://github.com/frgmt0/depper.git
cd depper
go build -o depper ./cmd/depper
```

To embed a version string, as release builds do:

```sh
go build -ldflags "-X github.com/frgmt0/depper/internal/version.Version=v0.2.0" -o depper ./cmd/depper
```

### Shell completion

`depper completion` generates completion scripts for bash, zsh, fish, and PowerShell. Run `depper completion SHELL --help` for the details of one shell. Write the script once to the location for your shell, then start a new shell.

bash (needs the `bash-completion` package):

```sh
mkdir -p ~/.local/share/bash-completion/completions
depper completion bash > ~/.local/share/bash-completion/completions/depper
```

zsh. Write the script as `_depper` into a directory on `$fpath` (it must come before `compinit` runs), and make sure completion is enabled:

```sh
mkdir -p ~/.zsh/completions
depper completion zsh > ~/.zsh/completions/_depper
```

```sh
# in ~/.zshrc, before compinit
fpath=(~/.zsh/completions $fpath)
autoload -U compinit; compinit
```

fish:

```sh
mkdir -p ~/.config/fish/completions
depper completion fish > ~/.config/fish/completions/depper.fish
```

To try completion in the current session only: `source <(depper completion bash)`, `source <(depper completion zsh)`, or `depper completion fish | source`. Add `--no-descriptions` to omit the descriptions.

### Updating

Re-run the install script, which installs the latest release over the existing binary (or pin a version with `DEPPER_VERSION`). If you installed with `go install`, run the same `go install ...@latest` command again. Regenerate the completion file after an update if commands changed.

### Uninstalling

Remove the binary and any completion files you created:

```sh
rm "$(command -v depper)"
rm -f ~/.local/share/bash-completion/completions/depper
rm -f ~/.zsh/completions/_depper
rm -f ~/.config/fish/completions/depper.fish
```

depper does not install anything else on your local machine: no services, daemons, or global configuration. Uninstalling the binary does not affect deployments already running on your hosts; see [Deployments](docs/deployments.md) for managing those.

## Quick start

```sh
cd your-repository
depper init                       # detects the project and writes a starter depper.toml
depper host init root@SERVER      # prepares a fresh VPS and writes the connection into depper.toml
depper deploy
```

`depper init` recognises Docker Compose, Dockerfile, Go, Node.js, Python, Rust, and static sites, and marks every inferred line with a comment saying where it came from; review them before you deploy ([Configuration](docs/configuration.md#generating-a-starter-file-with-depper-init)). `depper host init` connects as root (or a user with passwordless sudo), creates an unprivileged deploy user with your SSH key, enables lingering, installs `rsync` and, for container projects, podman or docker, pins the host key after you confirm its fingerprint, and records what it did on the host. Add `--dry-run` to see the plan without changing anything, and `depper host status` to check the result later. It supports Debian/Ubuntu and Fedora/RHEL hosts with systemd; only Ubuntu 24.04 has been verified on a real host (see [Support status](docs/hosts.md#support-status)).

Afterwards:

```sh
depper doctor         # checks SSH, rsync, systemd, linger, disk space
depper status
depper health
```

To use a host you prepared yourself, skip `host init`: create the account, enable lingering, and set `[deploy].host` in `depper.toml` by hand ([Manual setup](docs/hosts.md#manual-setup)). Use `depper deploy --dry-run` to validate the configuration without connecting.

A minimal `depper.toml`:

```toml
[service]
name = "basic"
command = "python3 server.py"

[deploy]
host = "deploy@203.0.113.10"

[env]
PORT = "8080"

[health]
url = "http://127.0.0.1:8080/health"
```

`depper` with no command is the same as `depper deploy`. A repository path can be given where the usage shows `[REPO]`, for example `depper status /path/to/repo`; `depper /path/to/repo status` is accepted as well.

The health URL is requested from the host's network view, so `127.0.0.1` refers to the host, not your machine.

## Commands

| Command | Purpose |
| --- | --- |
| `depper init [REPO]` | Detect the project type and write a starter `depper.toml`. Flags: `--print`, `--force`. |
| `depper host init ADMIN@HOST [REPO]` | Prepare a fresh Linux host over SSH (deploy user, key, linger, packages, optional hardening) and write the connection into `depper.toml`. Flags: `--dry-run`, `--yes`, `--user`, `--port`, `--key`, `--identity`, `--known-hosts`, `--container`, `--cloudflared`, `--harden-ssh`, `--firewall`, `--auto-updates`, `--tailscale-authkey-env`, `--no-write-config`. |
| `depper host status [REPO]` | Show how the host was prepared and check it as the deploy user. Exit 1 if a check fails. |
| `depper deploy [REPO]` | Create a release, activate it, wait for health. Flags: `--dry-run`, `--env FILE`, `--env-stdin`, `--ref REF`. |
| `depper status [REPO]` | Unit state, current release, endpoint, tunnel URL, and health. Exits 0 whenever the host could be read, even if the service is unhealthy. |
| `depper health [REPO]` | Run the health check. Flags: `--watch SECONDS`, `--count N`. Exit 2 when unhealthy. |
| `depper check [REPO]` | Compare the deployed commit and secrets with the local repository. Exit 3 when outdated, not deployed, or secrets differ. |
| `depper releases [REPO]` | List releases on the host, newest first. |
| `depper rollback [REPO]` | Switch to the previous release, or `--to ID`. After an interrupted operation whose automatic restore failed, a bare `rollback` restores the state saved before it. |
| `depper logs [REPO]` | Show the service journal. Flags: `--lines N`, `-f`. |
| `depper restart [REPO]`, `depper stop [REPO]` | Restart or stop the unit. |
| `depper secrets list\|diff\|push [REPO]` | Inspect and replace the service environment. |
| `depper doctor [REPO]` | Check that the host is ready. Exit 1 if any check fails. |
| `depper unlock [REPO]` | Remove a stale deployment lock (not refreshed for 15 minutes). `--force` for a more recent one. |
| `depper ci init [REPO]` | Write a GitHub Actions deploy workflow. |
| `depper tunnel`, `tunnel-status`, `tunnel-logs`, `tunnel-stop` | Manage the optional Cloudflare Tunnel. |
| `depper migrate [REPO]` | Convert a deployment made by the Python `deployer`; rerun it to resume an interrupted migration. |

Global flags: `--target NAME` (or `DEPPER_TARGET`) selects a `[targets.NAME]` overlay; `--json` writes machine-readable JSON to stdout and moves human messages to stderr.

Exit codes: 0 success, 1 error, 2 unhealthy (`health`), 3 drift, outdated, or not deployed (`check`, `secrets diff`).

## Documentation

- [Configuration reference](docs/configuration.md): project detection by `depper init`, every key in `depper.toml`, targets, validation.
- [Preparing a host](docs/hosts.md): `depper host init` and `host status`, hardening options, then the manual steps (accounts, SSH keys, known_hosts, linger, firewall) and `depper doctor`.
- [Deployments](docs/deployments.md): releases, the deploy transaction, automatic restore, rollback, locking.
- [Secrets](docs/secrets.md): dotenv format, storage, fingerprints, rotation, threat model.
- [Containers](docs/containers.md): docker and podman, image and compose modes.
- [Health checks and status](docs/health.md): `[health]`, `[endpoint]`, JSON schemas, polling long-running tasks.
- [CI/CD](docs/ci.md): GitHub Actions workflow and other CI systems.
- [Cloudflare Tunnel](docs/tunnels.md): optional public HTTPS routes.
- [Migrating from deployer](docs/migrating-from-deployer.md)
- [Development](docs/development.md): building, testing, releasing.
- [Changelog](CHANGELOG.md)
- Examples: [`examples/`](examples)

## License

depper is released under the [MIT License](LICENSE).
