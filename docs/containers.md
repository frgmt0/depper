# Containers

A service can run as a container instead of a process. depper still manages it as a systemd user unit, `depper-<name>.service`; the unit runs docker or podman in the foreground, so systemd supervises, restarts, and logs it like any other service.

`[container]` replaces `[service].command`; the two cannot be combined. All keys are listed in the [configuration reference](configuration.md#container).

## Modes

Exactly one of `build`, `image`, or `compose` selects the mode.

### build

```toml
[service]
name = "api"

[container]
engine = "docker"
build = "."
dockerfile = "Dockerfile"        # optional, relative to the repository
ports = ["127.0.0.1:8080:8080"]
volumes = ["/srv/api-data:/data"]
```

During the setup phase of a deploy depper runs, in the new release directory:

```sh
<engine> build -t depper/<name>:<release-id> --label depper.service=<name> [-f <dockerfile>] <build context>
```

Each release gets its own image tag, `depper/<name>:<release-id>` (the name is lowercased). The release's unit runs that tag. The label `depper.service=<name>` carries the exact service name; pruning uses it to find leftover images of this service and never touches images of another service, even one whose name lowercases to the same repository.

For `build` and `image`, the lowercased `[service].name` must be a valid image name (`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`); the configuration is rejected otherwise.

### image

```toml
[container]
engine = "podman"
image = "ghcr.io/example/app:1.4.2"
ports = ["127.0.0.1:8080:8080"]
```

During setup depper runs `<engine> pull <image>` and tags the pulled image as `depper/<name>:<release-id>`. The configured reference is recorded in the release metadata as `image`, and the repository digest of the pulled image (`repo@sha256:...`), when the engine reports one, as `image_digest` (`depper releases --json`, `depper status --json`). The unit runs the tagged image, so a release keeps running the exact image it was deployed with even if the upstream tag moves, and a rollback restores that image. For private registries, run `docker login` (or `podman login`) as the deploy account on the host; depper does not manage registry credentials. `image` takes a plain image reference, including digest references such as `name@sha256:...`.

### compose

```toml
[container]
engine = "docker"
compose = "compose.yaml"
```

depper uses the compose plugin (`<engine> compose`); `depper doctor` checks it (see [Host requirements](#host-requirements)). During setup it runs `compose config` with its output discarded (`>/dev/null`, to validate the file; not `--quiet`, which podman-compose does not accept) and `compose build` in the release directory. The unit runs `compose up --build --remove-orphans` and stops with `compose down`. Every one of these commands passes `--env-file /dev/null`, so a `.env` file in the repository or the release is ignored (see [How secrets reach containers](#how-secrets-reach-containers)). The compose project name is `depper-<name>`, lowercased, with `.` replaced by `_`. `ports`, `volumes`, `args`, and `command` are rejected in `depper.toml` in this mode; define them in the compose file.

## The generated unit

For build and image modes the unit looks like this (engine path and release id vary; the unit contains no values):

```ini
[Unit]
Description=depper: api
After=network.target

[Service]
Type=simple
WorkingDirectory=/home/deploy/.local/share/depper/api/current
EnvironmentFile=/home/deploy/.local/share/depper/api/.depper/environment
ExecStartPre=-/bin/sh -c "..." "/usr/bin/docker" "rm" "-f" "depper-api"
ExecStart=/bin/sh -c "..." "/usr/bin/docker" "run" "--rm" "--name" "depper-api" $DEPPER_CONTAINER_ENV_ARGS "-p" "127.0.0.1:8080:8080" "-v" "/srv/api-data:/data" "depper/api:release-20261012T154501.237Z-3fa9c2d1"
ExecStop=/bin/sh -c "..." "/usr/bin/docker" "stop" "depper-api"
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

For compose mode:

```ini
ExecStart=/bin/sh -c "..." "/usr/bin/docker" "compose" "-p" "depper-api" "-f" "compose.yaml" "--env-file" "/dev/null" "up" "--build" "--remove-orphans"
ExecStop=/bin/sh -c "..." "/usr/bin/docker" "compose" "-p" "depper-api" "-f" "compose.yaml" "--env-file" "/dev/null" "down"
```

(In the unit, `"..."` is the environment-sanitising script described below.)
Notes:

- `"..."` stands for a script that every engine invocation (`ExecStart`, `ExecStartPre`, and `ExecStop`, in all three modes) runs through `/bin/sh -c '<sanitise>; exec "$0" "$@"'`, with the engine path and its arguments as `$0` and `"$@"`. It applies the same environment sanitising as depper's own scripts: `XDG_RUNTIME_DIR` is reset to `/run/user/UID` unless the inherited one is owned by the user, and `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and `XDG_STATE_HOME` are kept only when the user can write to them (or when the directory is missing and its parent is writable). A service therefore does not inherit a foreign `XDG_*` from the systemd user manager, for example from `/etc/environment` through `pam_env`; rootless podman refuses a runtime directory it does not own. Process-mode services get the same treatment from `.depper-start.sh`. depper cannot change what the user manager itself inherits, so on images that put another user's `XDG_*` in `/etc/environment`, remove those lines as well (see [Preparing a host](hosts.md#another-users-xdg-variables-in-etcenvironment)).
- The container name is `depper-<name>`. `ExecStartPre=-... rm -f` removes a stale container left by a crash; the leading `-` means a failure there is ignored.
- depper resolves the engine's absolute path on the host with `command -v` and writes it into the unit.
- The order of `run` arguments is: `--rm --name`, the `--env KEY` arguments from `$DEPPER_CONTAINER_ENV_ARGS`, `-p` entries, `-v` entries, `args`, the image, then the `command` words.
- `args` are passed through verbatim to `<engine> run`. Anything there (`--network`, `--cap-add`, `--privileged`) is your responsibility.
- `command` is split into words with POSIX quoting rules and no expansion, and appended after the image.
- systemd, not the engine, restarts the service (`restart` in `[service]` is honored). There is no `--restart` flag.
- The unit does not depend on the docker daemon, which is a system service. With lingering enabled the user manager can start before the daemon is ready; the unit's restart policy retries the start.

## Ports

Entries are `host:container`, `ip:host:container`, or `[ipv6]:host:container`, with an optional `/tcp` or `/udp`. Prefer an explicit loopback address, `127.0.0.1:8080:8080`, unless the service must be reachable from other machines. Docker publishes ports through its own firewall rules, which can bypass host firewall tools such as ufw; a bare `8080:8080` listens on all interfaces. See [Preparing a host](hosts.md#exposing-a-port).

Because [health checks](health.md) run on the host, a URL check against a loopback-published port (`http://127.0.0.1:8080/health`) works without exposing the port.

## Volumes

Entries are `/absolute/host/path:/container/path`, optionally with `:ro` or `:rw`. Both paths must be plain absolute paths (no spaces, quotes, backslashes, percent signs, or commas). Create the host directory beforehand with the right ownership; depper does not create it. Put persistent data in volumes: release directories are replaced on every deploy and pruned later.

## How secrets reach containers

There is no container env-file (older depper versions wrote `<base>/.depper/container.env`; the next `depper deploy` removes it). Values never pass through a docker or podman env-file or an env-file parser, so quotes, backslashes, `$`, `#`, and newlines in values arrive exactly as written, and multi-line and quoted values are allowed.

- Build and image modes: systemd reads `<base>/.depper/environment` through `EnvironmentFile=` and puts the variables into the environment of the engine client process. The same file carries one more variable, `DEPPER_CONTAINER_ENV_ARGS`, whose value is the list `--env KEY` for every key, for example `--env API_TOKEN --env LOG_LEVEL`. The unit expands it, unquoted, in `ExecStart=`, so systemd splits it into words and the engine receives `--env KEY` with no value. The engine reads each value from its own environment and sets it in the container. Because the key list lives in the environment file and not in the unit, `depper secrets push` can add and remove keys without changing the unit. `DEPPER_CONTAINER_ENV_ARGS` is reserved: it is not a user variable, is not fingerprinted or listed, and a variable of that name in `[env]` or the dotenv file is an error.
- Compose mode: every compose command depper runs uses `--env-file /dev/null`, so compose reads no `.env` file from the repository or the release. `${VAR}` in the compose file is interpolated from depper's environment: at runtime the unit's `EnvironmentFile=` (both `[env]` and secrets), and during setup (`compose config` and `compose build`) the same values, which depper exports in the setup script on the host (sent on standard input, never in a command line). A service receives a variable only if it names it in its `environment:` section, for example:

```yaml
services:
  api:
    build: .
    environment:
      API_TOKEN: ${API_TOKEN}          # mapping form: interpolated from depper's environment
      LOG_LEVEL: ${LOG_LEVEL:-info}    # with a default when the variable is unset
```

or, equivalently, the list form that passes variables through by name:

```yaml
    environment:
      - API_TOKEN
      - DATABASE_URL
```

Do not use `env_file: .env` for secrets: depper does not upload `.env` files.

Limits, for all container modes including compose: a value may be at most 60 KiB, because values travel in the engine's process environment and its API request. A larger value is rejected before anything is sent to the host, and the error names the key and never the value.

Existing container deployments made by an earlier depper (whose unit reads `container.env`) must run `depper deploy` once before `depper secrets push`; until then `secrets push` stops with an error that says so. Rolling back to a release that an earlier depper created restores that release's own unit and `container.env`, and `secrets push` is then refused again until the next `depper deploy`.

Visibility: the container's environment is stored by the engine, so `docker inspect` or `podman inspect` shows it to anyone who can reach the engine. For docker that includes every member of the `docker` group, who are root-equivalent already (see below). [Secrets](secrets.md#threat-model) describes the wider model.

## Rollback

`depper rollback` restores the unit, environment, and `current` link of the target release and restarts.

- build mode: the restored unit runs that release's own image tag, `depper/<name>:<release-id>`, which still exists as long as the release is kept. No rebuild happens.
- image mode: the restored unit runs the tag created from the image pulled for that release, so the rollback runs exactly what that release ran, not whatever the upstream tag points to now.
- compose mode: depper does not tag images. The restored unit runs `compose up --build` from the previous release's files. Locally built services are rebuilt from that release's sources (usually from the build cache), and services that reference a registry tag resolve that tag again. Pin images by digest in the compose file if you need rollbacks to be exactly reproducible.

Pruning a release deletes its image tag (`rmi`), and also removes images labelled `depper.service=<name>` for this service whose release no longer exists. A failed deploy removes the image it built; a deploy interrupted with Ctrl-C leaves the release, and its image, until the next successful deploy prunes the discarded release. Volumes and named compose volumes are never removed by depper.

## Host requirements

`depper host init` installs the engine for you. Without `--container`, it installs the engine that `depper.toml` resolves `[container].engine` to (`docker` when the key is unset) and nothing when there is no `[container]`; `--container podman|docker|none` overrides that. `depper init` writes `engine = "podman"` for container projects, so a detected container project gets rootless podman by default. For podman it also installs the rootless helpers and adds subordinate id ranges for the deploy user; for docker it adds the deploy user to the `docker` group, which is root-equivalent (see below), and enables `docker.service`. docker is refused on RHEL, CentOS, Rocky, and AlmaLinux, which have no distribution package. When the resolved configuration uses `compose`, `host init` also installs a compose provider: `podman-compose` for podman; for docker, the first of `docker-compose-plugin`, `docker-compose-v2`, and `docker-compose` (version 2 or later only) that is installed or available from the repositories. If none is available it refuses with a message: with empty apt lists, run `sudo apt-get update`; on RHEL-compatible systems `podman-compose` is in EPEL; docker on Debian bookworm only offers compose v1, so install `docker-ce` and `docker-compose-plugin` from Docker's repository. Podman is refused on Amazon Linux 2023; use `--container docker` there. The details and the support status are in [Preparing a host](hosts.md#container-engine). Only podman on Ubuntu 24.04 has been verified on a real host.

Requirements, whether you use `host init` or prepare the host yourself:

- The engine binary (`docker` or `podman`) installed on the host, found on the non-interactive SSH `PATH`. `depper doctor` fails if it is missing.
- Compose mode needs the compose plugin: `docker compose` (v2), or a working `podman compose` provider. `depper doctor` has a `compose` check for this: in compose mode, when the engine is installed, it runs `<engine> compose version` as the deploy user and fails if that does not succeed. It is skipped when the engine itself is missing (the `container engine` check already fails).
- The account must be able to use the engine without root:
  - Rootless podman: no daemon and no extra group. Requires subordinate UID/GID ranges for the account (`/etc/subuid`, `/etc/subgid`), and lingering so containers survive logout. Rootless containers cannot bind ports below 1024 unless the system allows it.
  - Docker: the account must be in the `docker` group, or use rootless docker.

**Docker group membership is root-equivalent.** Anyone who can talk to the docker daemon can start a container that mounts the host's filesystem and write to it as root. Adding the deploy account to the `docker` group therefore gives that account, and anyone holding its deploy key, full control of the machine. Where that is not acceptable, use rootless podman or rootless docker.

Health checks run on the host, not inside the container, so a URL check reaches the service only through a published port. To check from inside the container, use a [command check](health.md#command-checks), for example `docker exec depper-api wget -qO- http://127.0.0.1:8080/health` (the image must contain the tool you call).
