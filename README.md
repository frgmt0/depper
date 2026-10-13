# deployer

`deployer` is a small Python 3.11+ command-line tool for shipping a repository to a Linux host over SSH and rsync, then running it as a systemd **user** service. It keeps the service definition in the repository and uses the host's user account, so the deployed process does not need root privileges.

## Install

From a checkout, install the command with either:

```sh
pipx install .
```

or an isolated virtual environment:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install .
```

Your local `python3` must be version 3.11 or newer. The target host must be Linux with Python 3 and rsync available. SSH must be able to connect using the configured host name or alias. See [SETUP.md](SETUP.md) for host preparation.

## Quick start

The repository needs a `deploy.toml` file. This example deploys a Python HTTP server:

```toml
[service]
name = "sample-http"
command = "python3 server.py"
restart = "on-failure"

[deploy]
host = "desktop"
# path = "/srv/sample-http" # optional; omit to use ~/.local/share/deployer/sample-http

[env]
PORT = "8080"
```

```sh
deployer init       # create a starter deploy.toml when one is absent
deployer            # deploy (the default command)
deployer status
deployer logs
deployer restart
deployer stop
deployer rollback
```

Use a repository path explicitly when running the command elsewhere. The subcommand comes first:

```sh
deployer deploy /path/to/sample-http
deployer deploy --dry-run
deployer deploy --env .env.prod
```

`--dry-run` shows the planned deployment without copying files or changing the remote service. `status`, `logs`, `restart`, `stop`, and `rollback` operate on the service named by `deploy.toml`. Rollback uses the deployer's retained previous release when one is available; it cannot roll back database or other external state.

`--env FILE` overrides the file selected by `[secrets].file` for that deployment. Relative CLI paths start at your current directory; relative TOML paths start at the repository root. Absolute paths and `~` are also supported.

`--env FILE` is a deploy-only option; status, logs, restart, stop, rollback, and tunnel commands do not load secret files. If a secret source is removed, the next deployment writes only the remaining `[env]` values and removes keys that existed only in the old secret source.

## Configuration

`deploy.toml` has three core sections and optional `[secrets]` and `[tunnel]` sections:

- `[service]` requires `name` and `command`. `restart` is optional and must be `on-failure`, `always`, or `no`.
- `[deploy]` requires `host`. `path` is an optional absolute remote directory. If omitted, the path is resolved on the remote host as `~/.local/share/deployer/<name>`. `setup` is an optional array of shell commands run on the remote host before starting the service. `exclude` is an optional array of rsync exclude patterns. `healthcheck` is an optional command used after startup; a nonzero exit status marks deployment unhealthy.
- `[env]` contains string environment variables passed to the service.
- `[secrets]` is optional. Set `file` to a dotenv file path such as `.env.prod`; repository-relative paths are resolved from the repository, while a CLI `--env FILE` path is resolved from the current working directory.
- `[tunnel]` is optional. Set `hostname` to a chosen `*.frgmt.xyz` hostname and `service` to the local origin (for example, `http://127.0.0.1:3001`). The deployer creates a dedicated locally-managed `cloudflared` tunnel for the service and runs it as an independent user service.

For example:

```toml
[service]
name = "worker"
command = ".venv/bin/python worker.py --queue jobs"
restart = "always"

[deploy]
host = "desktop"
path = "/home/deploy/apps/worker"
setup = [
  "python3 -m venv .venv",
  ".venv/bin/pip install -r requirements.txt",
]
exclude = [".git/", ".venv/", "__pycache__/"]
healthcheck = "curl -fsS http://127.0.0.1:8080/ >/dev/null"

[env]
PYTHONUNBUFFERED = "1"
QUEUE_NAME = "jobs"

[secrets]
file = ".env.prod"
```

An optional public route looks like this:

```toml
[tunnel]
hostname = "worker.frgmt.xyz"
service = "http://127.0.0.1:8080"
```

The hostname must be available in your Cloudflare zone. The deployer never overwrites an existing DNS route or takes over a tunnel it does not own; choose another hostname if there is a conflict. A public HTTPS route and access control are separate decisions: configure [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/access-controls/) when the application must require authentication.

Keep secrets out of `deploy.toml` and version control. During `deploy`, values from `[secrets].file` or `--env FILE` override duplicate `[env]` keys and are transferred over SSH to the protected remote `.deployer/environment` file (directories mode 700, file mode 600). Both the configured dotenv file and a CLI override are excluded from rsync—including an anchored path for a selected file inside the repository, and its resolved target when applicable—and are never copied into the release or left in the service working directory. Secrets are loaded only for the runtime service; setup and healthcheck commands do not receive them. There is no interpolation or shell sourcing. Double-quoted values may contain escaped or literal newlines; unquoted values are single-line. A missing selected file fails deployment rather than silently reusing a previous copy. Previous values remain in protected rollback transaction backups, so rotate secrets with that retention caveat.

The dotenv file accepts one `KEY=VALUE` assignment per line, with blank lines and comments beginning with `#` ignored. An optional `export ` prefix is accepted. Keys use shell environment names; values may be unquoted or enclosed in matching single or double quotes. Double-quoted values support `\n`, `\r`, `\t`, `\\`, and `\"` escapes and may span lines; single-quoted and unquoted values do not interpret escapes. Inline comments beginning with whitespace followed by `#` are ignored for unquoted values. Files must be UTF-8 (a UTF-8 BOM is accepted) and no larger than 1 MiB. Shell expressions, command substitutions, and `${NAME}` references remain literal text; they are never evaluated.

The protected environment file is a permission boundary, not an encrypted vault: the service account can read it, and root can read it. Application code can also expose values through its own logs or diagnostics.

## Commands

```text
deployer [COMMAND] [REPO]
deployer deploy [REPO] [--dry-run] [--env FILE]
deployer init [REPO]
deployer status [REPO]
deployer logs [REPO]
deployer restart [REPO]
deployer stop [REPO]
deployer rollback [REPO]
deployer tunnel [REPO]
deployer tunnel-status [REPO]
deployer tunnel-logs [REPO] [--lines N]
deployer tunnel-stop [REPO]
```

Run `deployer --help` for the installed command's complete option list. Targets are Linux systemd user services; this tool does not manage Windows services, macOS launchd jobs, containers, or system-wide systemd units.

Cloudflare tunnels are optional and require separate `cloudflared` authentication on the target host. See [SETUP.md](SETUP.md#optional-cloudflare-tunnel) before adding a `[tunnel]` section.

When a tunnel is configured, manage its independent connector with:

```sh
deployer tunnel
deployer tunnel-status
deployer tunnel-logs
deployer tunnel-stop
```

`tunnel-stop` disables the connector while retaining its owned state and DNS route; the next `deployer deploy` starts it again. Tunnel hostname and origin changes are refused in this version. A tunnel setup failure can leave the application deployment successful, so check both the application and tunnel status.

## Deployment behavior

Each deployment copies the current working tree, including uncommitted changes, into a new release directory. It runs setup, installs the user service, then checks that the process is running without automatic restarts after three seconds. An optional health command must also succeed. This causes a short service interruption; it is not a zero-downtime deployment.

`restart = "no"` disables crash restarts; the service is still enabled to start at boot. Use `on-failure` for crash recovery or `always` to also restart after a clean process exit. A deliberate `deployer stop` stays stopped until restarted or the user manager starts again.

Keep databases, uploads, and other persistent data outside release directories. Old and failed releases are retained; this version does not automatically prune disk usage. Custom remote paths use plain absolute paths without spaces, quotes, backslashes, or percent signs. Use an SSH alias for IPv6 hosts or custom ports.
