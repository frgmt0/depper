# Configuration reference

depper reads one TOML file from the repository root.

- The file is `depper.toml`. If it is absent, `deploy.toml` (the name used by the Python `deployer`) is read instead. If both exist, depper stops with an error; keep only `depper.toml`.
- `depper init` writes a starter `depper.toml` and refuses to run when either file exists.
- Unknown tables and unknown keys are errors, so a misspelled key is reported rather than ignored.
- Both file names are excluded from the upload to the host.
- Relative local paths in the file (`identity_file`, `known_hosts`, `[secrets].file`) are relative to the repository root. `~` is expanded. Paths given on the command line (`--env FILE`) are relative to the current directory.

Top-level tables: `[service]`, `[deploy]`, `[env]`, `[secrets]`, `[tunnel]`, `[container]`, `[endpoint]`, `[health]`, and `[targets.<name>]`. `[service]` and `[deploy]` are required.

A complete example:

```toml
[service]
name = "worker"
command = ".venv/bin/python worker.py"
restart = "always"

[deploy]
host = "deploy@203.0.113.10"
port = 22
identity_file = "~/.ssh/id_ed25519_deploy"
known_hosts = "~/.ssh/depper_known_hosts"
path = "/home/deploy/apps/worker"
setup = ["python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"]
exclude = ["tests/", "docs/"]
keep_releases = 5
source = "worktree"

[env]
LOG_LEVEL = "info"

[secrets]
file = ".env.production"

[endpoint]
url = "http://203.0.113.10:8080"

[health]
url = "http://127.0.0.1:8080/health"
expect_status = 200
timeout = 5
start_period = 30
interval = 2
```

## [service]

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `name` | string | required | Service name. Used in the unit name `depper-<name>.service`, the default remote path, and the container and image names. Must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`. |
| `command` | string | required unless `[container]` is set | Shell command that starts the service. It is run as `exec <command>` by `/bin/sh` from the release directory, so shell syntax (pipes, `&&`, variable references) works, and the command becomes the unit's main process. Must be a single line. Mutually exclusive with `[container]`; use `[container].command` to override the image command. |
| `restart` | string | `"on-failure"` | systemd `Restart=` value: `"on-failure"`, `"always"`, or `"no"`. With `"no"` the unit is still enabled and starts at boot (when lingering is enabled). |

The unit sets `RestartSec=3`. `depper stop` leaves the service stopped until `depper restart`, the next deploy, or a reboot of the user manager.

## [deploy]

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `host` | string | required | SSH destination: an alias from `~/.ssh/config`, a DNS name, an IPv4 address, or an IPv6 literal (bare or in brackets), optionally prefixed with `user@`. Names must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`. |
| `user` | string | none | SSH login name, passed as `-l`. Must not be combined with a `user@` prefix in `host`. Must match `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`. |
| `port` | integer | ssh default | SSH port, 1 to 65535, passed as `-p`. |
| `identity_file` | string | ssh default | Local private key. Passed as `-i` together with `-o IdentitiesOnly=yes`. The resulting absolute path (after `~` expansion and resolving against the repository root) may not contain whitespace or control characters. |
| `known_hosts` | string | ssh default | Local known_hosts file. Passed as `-o UserKnownHostsFile=<file> -o StrictHostKeyChecking=yes`, so only the host keys in that file are accepted. The resulting absolute path may not contain whitespace or control characters: ssh splits this option at whitespace, so such a path cannot be quoted. |
| `path` | string | `~/.local/share/depper/<name>` | Absolute remote base directory. May contain only letters, digits, and `. _ @ + / -` (`^/[A-Za-z0-9._@+/-]+$`) and may not end with `/`. When omitted the default is resolved against the remote `$HOME`, which must then consist of the same characters; if it does not, depper stops with an error and you must set `path`. The restriction exists because the path is used unquoted in rsync destinations and unit files. |
| `setup` | array of strings | `[]` | Commands run on the host, in order, in the new release directory before activation. Each runs as `/bin/sh -lc "<command>"` with `[env]` exported. Secrets are not exported. A non-zero exit aborts the deployment. No newlines. |
| `exclude` | array of strings | `[]` | Additional rsync exclude patterns. Must be relative: no leading `/`, no `..` component, no newline. Not overridable per target. |
| `keep_releases` | integer | `5` | Number of releases to keep, at least 2. The current and previous releases are always kept. |
| `source` | string | `"worktree"` | `"worktree"` uploads the working directory including uncommitted changes. `"git"` uploads the committed content of `HEAD` (`git archive`). `depper deploy --ref REF` forces git mode for one deployment. |
| `healthcheck` | string | none | Alias of `[health].command`. Setting both is an error. |

Always-excluded rsync patterns (applied before `exclude`): `.git/`, `.env`, `.env.*`, `deploy.toml`, `depper.toml`, `.cloudflared/`, `.cache/`, `node_modules/`, `venv/`, `.venv/`, `__pycache__/`, `.pytest_cache/`, `.mypy_cache/`, `.ruff_cache/`. The selected secrets file is also excluded by its anchored path when it lies inside the repository. `.gitignore` is not consulted in worktree mode: anything not excluded is uploaded, including ignored build output.

Examples of `host`:

```toml
host = "deploy-box"                 # alias from ~/.ssh/config
host = "deploy@203.0.113.10"        # user and IPv4
host = "2001:db8::10"               # IPv6 literal
host = "app.example.com"
user = "deploy"                     # equivalent to deploy@app.example.com
```

### Environment overrides for credentials

`DEPPER_SSH_KEY_FILE` and `DEPPER_KNOWN_HOSTS_FILE` override `identity_file` and `known_hosts` for any target. They exist for CI, where the key and host key are written to temporary files. An empty value is ignored. Values may not contain control characters, and the resulting absolute path may not contain whitespace (as for `identity_file` and `known_hosts`); `~` is expanded.

### SSH behaviour

Every ssh and rsync invocation uses `-o BatchMode=yes`, so depper never prompts for a password or a host key confirmation. An unknown host key makes the connection fail unless you have connected once with plain `ssh` or provide `known_hosts`. Everything else, including `ProxyJump`, `ControlMaster`, and `IdentityAgent`, comes from your normal ssh configuration. Scripts run without a terminal (`ssh -T`) and arrive on the remote `sh -s` standard input wrapped in one `{ ... }` group, which the remote shell parses completely before it runs any of it. A connection that drops while a script is being sent therefore runs nothing, instead of a truncated prefix. The `ssh -W` connection used by URL health checks runs with `ControlPath=none` and does not use connection multiplexing (see [Health checks](health.md#url-checks)).

## [env]

String variables for the service. Keys must match `^[A-Za-z_][A-Za-z0-9_]*$`, and values must be single-line strings.

```toml
[env]
PORT = "8080"
LOG_LEVEL = "info"
```

`[env]` values are written to the service environment, exported during `setup`, and exported for command health checks. They are stored on the host and are visible to anyone who can read your repository, so do not put secrets here. Values from the dotenv file override `[env]` keys of the same name.

With `[container]`, the name `DEPPER_CONTAINER_ENV_ARGS` is reserved and every value, from `[env]` or from the dotenv file, may be at most 60 KiB; a violation stops the command and the error names the key, never the value. See [Containers](containers.md#how-secrets-reach-containers).

## [secrets]

| Key | Type | Description |
| --- | --- | --- |
| `file` | string | Local dotenv file, relative to the repository root, absolute, or starting with `~`. Required when the table is present. |

```toml
[secrets]
file = ".env.production"
```

`depper deploy --env FILE` or `--env-stdin` replaces the configured file for one invocation. A selected file that does not exist is an error; depper never silently reuses an older copy. See [Secrets](secrets.md).

## [endpoint]

| Key | Type | Description |
| --- | --- | --- |
| `url` | string | Address where the service is reachable without a tunnel (LAN, Tailscale, VPS address). Informational: printed after a deploy and shown by `depper status`. It is not used for health checks. |

The URL must use `http` or `https`, include a host, and contain no spaces, credentials, or fragment. A port, if present, must be 1 to 65535.

## [health]

Exactly one of `url` or `command` must be set when the table is present. See [Health checks and status](health.md) for behaviour.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `url` | string | none | HTTP or HTTPS URL requested from the host's network view. Same URL rules as `[endpoint].url`. |
| `command` | string | none | Shell command run on the host in the current release with `[env]` exported. Exit status 0 is healthy. No newlines. |
| `expect_status` | integer | any 2xx | Required HTTP status, 100 to 599. URL checks only. |
| `timeout` | number | `5` | Seconds allowed for one check. Greater than 0, at most 86400. For URL checks it bounds the HTTP exchange only; the SSH connection gets `max(15 s, timeout)` (see [URL checks](health.md#url-checks)). |
| `start_period` | number | `30` | Seconds a deployment waits for the first passing check. 0 to 86400. |
| `interval` | number | `2` | Seconds between checks while waiting. Greater than 0, at most 86400. |

Durations are numbers of seconds; integers and decimals (`0.5`) are accepted.

Without `[health]`, a deployment waits three seconds and then requires the unit to be active with no restarts.

## [container]

Runs the service as a container instead of a process. Mutually exclusive with `[service].command`. Exactly one of `build`, `image`, or `compose` is required. See [Containers](containers.md).

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `engine` | string | `"docker"` | `"docker"` or `"podman"`. |
| `build` | string | | Build context directory, relative to the repository. Plain relative path without `..`. |
| `dockerfile` | string | engine default | Dockerfile path relative to the repository. Requires `build`. |
| `image` | string | | Prebuilt image reference to pull, for example `ghcr.io/example/app:1.4.2`. |
| `compose` | string | | Compose file, relative to the repository. |
| `ports` | array of strings | `[]` | Published ports: `host:container` or `ip:host:container` (IPv4, or `[ipv6]:host:container`), optional `/tcp` or `/udp` suffix. |
| `volumes` | array of strings | `[]` | `/absolute/host/path:/container/path`, optionally `:ro` or `:rw`. Both paths must be plain absolute paths. |
| `args` | array of strings | `[]` | Extra arguments for `<engine> run`, inserted before the image. No newlines. |
| `command` | string | image default | Overrides the image command. Split into words with POSIX quoting rules, without expansion. |

`ports`, `volumes`, `args`, and `command` are not allowed with `compose`; configuration is rejected if any of them is set, so set them in the compose file. For `build` and `image`, each release is tagged `depper/<name>:<release-id>`, so `[service].name` lowercased must be a valid image name: `^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$` (lowercase letters and digits, separated by one `.`, one `_`, `__`, or one or more `-`). The check applies after any target `name` override.

There is no `env` key in `[container]`. In `build` and `image` mode the container receives the variables from `[env]` and the dotenv file by name (`--env KEY`), with values from the unit's environment; in `compose` mode the compose file chooses. See [Containers](containers.md#how-secrets-reach-containers).

## [tunnel]

Optional Cloudflare Tunnel. See [Cloudflare Tunnel](tunnels.md). Not available as a target overlay.

| Key | Type | Description |
| --- | --- | --- |
| `hostname` | string | Exact public hostname with at least three labels, for example `app.example.com`. Lowercased; a trailing dot is removed. |
| `service` | string | Origin as seen from the host: `http://` or `https://`, a host, and an explicit port, with no path, query, fragment, or credentials. |

## Targets

`[targets.<name>]` defines an overlay on the base configuration, for staging and production style setups. Target names match `^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`.

Select a target with the global `--target NAME` flag or the `DEPPER_TARGET` environment variable. The flag wins.

Keys allowed in a target:

| Key | Merge behaviour |
| --- | --- |
| `name` | Replaces `[service].name`. Give each target on the same host its own name; units, paths, and containers are derived from it. |
| `host`, `user`, `port`, `identity_file`, `known_hosts`, `path`, `keep_releases`, `source` | Replace the base value. |
| `setup` | Replaces the whole base list when present. |
| `env` (sub-table) | Merged over base `[env]`, key by key. |
| `secrets` (sub-table, `file`) | Replaces base `[secrets]`. |
| `endpoint` (sub-table, `url`) | Replaces base `[endpoint]`. |
| `health` (sub-table, any key) | Keys merge over the base. Setting `url` or `command` replaces both base `url` and `command`. |

Not available in a target: `command`, `restart`, `exclude`, `healthcheck`, `[container]`, and `[tunnel]`. These come from the base configuration.

```toml
[service]
name = "api"
command = "./bin/api"

[deploy]
setup = ["make build"]

[env]
LOG_LEVEL = "info"

[health]
url = "http://127.0.0.1:8080/health"

[targets.staging]
host = "deploy@203.0.113.20"
name = "api-staging"

[targets.staging.env]
LOG_LEVEL = "debug"

[targets.production]
host = "deploy@203.0.113.10"
keep_releases = 10

[targets.production.secrets]
file = ".env.production"

[targets.production.endpoint]
url = "http://203.0.113.10:8080"
```

Rules:

- A base `[deploy].host` is optional when targets are defined. If targets exist, no base host is set, and no target is selected, depper stops and lists the target names.
- If a selected target does not set `host` and there is no base host, depper reports that the target needs a host.
- An unknown target is an error that lists the defined target names.
- Each target is a separate deployment with its own remote directory, unit, release history, lock, and secrets.

## Validation summary

depper validates the whole file on every command, including `--dry-run`, before contacting a host. Values that are interpolated into remote scripts or unit files are checked and quoted. Errors name the offending key, for example `[deploy].keep_releases must be an integer >= 2`. Error text never includes secret values.
