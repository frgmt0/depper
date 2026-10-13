# Preparing a host

depper needs a Linux host that you can reach over SSH, with a systemd user manager for the account that runs the service. This page describes the host side. Package names and commands are for Debian and Ubuntu; adapt them for other distributions.

## Requirements

- Linux with systemd. The service runs under `systemctl --user`, so `systemd` must be the init system and the account needs a user manager.
- A POSIX `sh` as the login shell's `sh -s` target, and GNU-compatible coreutils. depper uses `sha256sum`, `mktemp`, `base64 -d`, `readlink`, `ln -s`, `mv -T`, `find`, `head`, and `df`.
- `rsync`.
- `curl` is optional. It is used only if SSH TCP forwarding is refused for [URL health checks](health.md).
- Whatever your application needs: a language runtime, or docker/podman for [container deployments](containers.md).
- A remote `$HOME` (or a `[deploy].path` that you set) made only of letters, digits, and `. _ @ + / -`. The default base directory is `$HOME/.local/share/depper/<name>`, and depper uses base paths unquoted in rsync destinations and unit files. If the account's home directory contains other characters, such as a space, set `[deploy].path` to an absolute path that meets this rule; depper otherwise stops with an error that says so.

depper never uses root and never writes outside the account's home directory, apart from a custom `[deploy].path` that you choose.

## Create a dedicated account

Use an unprivileged account for deployments. It needs write access to its own application directory and a user systemd manager, nothing more.

```sh
sudo adduser --disabled-password --gecos '' deploy
sudo apt update
sudo apt install -y openssh-server rsync
```

Do not add the account to `sudo`. If you use docker, read the note on group membership in [Containers](containers.md#host-requirements) first.

## Enable lingering

User services normally stop when the last login session ends and do not start at boot. Lingering keeps the user manager running:

```sh
sudo loginctl enable-linger deploy
loginctl show-user deploy -p Linger        # Linger=yes
```

This is usually the only step that needs an administrator. Without it, services stop when your SSH session closes, and `depper doctor` reports a warning.

## SSH access

depper runs `ssh` and `rsync` as subprocesses, so your `~/.ssh/config`, agent, `ProxyJump`, and `ControlMaster` settings all apply. Connections use `BatchMode=yes`: there are no password or host key prompts. Local key and known_hosts file paths (`identity_file`, `known_hosts`, and the CI variables `DEPPER_SSH_KEY_FILE` and `DEPPER_KNOWN_HOSTS_FILE`) may not contain whitespace.

### Generate a deploy key

```sh
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_deploy -C "depper deploy key"
```

Install the public key for the deploy account, appending to existing keys. As an administrator on the host:

```sh
sudo install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
sudo sh -c 'cat >> /home/deploy/.ssh/authorized_keys' < ~/.ssh/id_ed25519_deploy.pub
sudo chown deploy:deploy /home/deploy/.ssh/authorized_keys
sudo chmod 600 /home/deploy/.ssh/authorized_keys
```

(`ssh-copy-id -i ~/.ssh/id_ed25519_deploy.pub deploy@203.0.113.10` does the same from your workstation if password login is still enabled.) Use a separate key for CI so it can be revoked independently.

### Choose how to name the host

A VPS by IP address:

```toml
[deploy]
host = "deploy@203.0.113.10"
identity_file = "~/.ssh/id_ed25519_deploy"
```

A private host on Tailscale (use the tailnet name or address):

```toml
[deploy]
host = "deploy@deploy-box.tailnet-name.ts.net"
```

An alias defined in `~/.ssh/config`:

```sshconfig
Host deploy-box
    HostName 203.0.113.10
    User deploy
    IdentityFile ~/.ssh/id_ed25519_deploy
    IdentitiesOnly yes
```

```toml
[deploy]
host = "deploy-box"
```

An alias works on your workstation but not in CI, where your ssh config does not exist. For CI, use a [target](configuration.md#targets) with a real address. Tailscale gives you private reachability; it does not replace SSH authentication or host hardening, and a CI runner must be able to reach the tailnet address.

Verify before the first deploy:

```sh
ssh deploy@203.0.113.10 'id -un && uname -s && systemctl --user --version | head -n 1'
```

### Host keys and known_hosts

By default ssh uses `~/.ssh/known_hosts` and, because depper runs ssh in batch mode, fails on an unknown host. Connect once with plain `ssh` and verify the fingerprint, or pin keys explicitly:

```sh
ssh-keyscan -p 22 203.0.113.10 > ~/.ssh/depper_known_hosts
ssh-keygen -lf ~/.ssh/depper_known_hosts        # compare with the provider console or the host's own key
```

```toml
[deploy]
host = "deploy@203.0.113.10"
known_hosts = "~/.ssh/depper_known_hosts"
```

With `known_hosts` set, depper passes `UserKnownHostsFile=<file>` and `StrictHostKeyChecking=yes`: only the keys in that file are accepted. The file entry must match the `host` value (an IP address entry does not match a DNS name). `ssh-keyscan` output is not authenticated by itself; check the fingerprint through a trusted channel.

## systemd user units

depper installs one unit per service in `~/.config/systemd/user/`:

- `depper-<name>.service` for the application
- `depper-tunnel@<name>.service` for an optional [Cloudflare Tunnel](tunnels.md) connector

Useful commands on the host, as the deploy account:

```sh
systemctl --user status depper-myapp.service
journalctl --user-unit depper-myapp.service -n 100
systemctl --user list-units 'depper-*'
```

Over SSH, set `XDG_RUNTIME_DIR` if it is not defined (depper does this itself):

```sh
ssh deploy@203.0.113.10 'export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user status depper-myapp.service'
```

`depper logs` and `depper status` cover the common cases.

## Exposing a port

depper does not configure a firewall. A service that binds `0.0.0.0` or a published container port is reachable from whatever the host's network allows. To make a service reachable from outside:

1. Bind it to the address you intend (`127.0.0.1` for local-only, or all interfaces).
2. Open the port in the host firewall (for example `sudo ufw allow 8080/tcp`) and in the provider's network firewall if there is one.
3. Record the address in `[endpoint]` so `depper status` shows it.

Prefer binding to `127.0.0.1` and putting TLS and authentication in front of the service (a reverse proxy, Tailscale, or a [Cloudflare Tunnel](tunnels.md)). Health checks run on the host, so they work with a loopback-only service.

## Verify with depper doctor

```sh
depper doctor
```

`doctor` runs one SSH session and reports `pass`, `warn`, or `fail` for each item:

| Check | Fails or warns when |
| --- | --- |
| `ssh` | The host cannot be reached or does not run POSIX sh scripts (fail). |
| `rsync` | `rsync` is not installed on the host (fail). |
| `sha256sum` | `sha256sum` is not installed (fail). |
| `systemd` | `systemctl --user` cannot reach the user manager (fail). |
| `linger` | Lingering is disabled or cannot be determined (warn). |
| `disk` | Less than 1 GiB is free on the filesystem that holds the base directory (warn). |
| `container engine` | docker or podman is not installed; only checked with `[container]` (fail). |
| `cloudflared` | `cloudflared` is not installed; only checked with `[tunnel]` (fail). |
| `lock` | A deployment lock is held; it reports the holder and "last refreshed X ago". A lock not refreshed for 15 minutes is stale and warns; an active one passes (see [Deployments](deployments.md#locking)). |
| `layout` | A deployment made by the Python `deployer` exists and needs `depper migrate` (`legacy`, warn); a `depper migrate` was interrupted and must be run again (`migrating`, warn); both a depper and a legacy deployment exist (`both`, warn). See [Migrating from deployer](migrating-from-deployer.md). |

The exit status is 1 if any check fails, otherwise 0. `depper doctor --json` prints `{"checks": [{"name", "status", "detail"}, ...]}`.

## Security notes

- Keep the deploy account narrow. Anyone who can use the deploy key can run arbitrary commands as that account, because `[service].command`, `setup`, and health commands are shell commands.
- Do not put secrets in `depper.toml` or in the repository. Use a [dotenv file](secrets.md).
- Secrets on the host are protected by file permissions (mode 600 inside a mode 700 directory) and are readable by the service account and by root.
- Take backups of application data yourself; depper's rollback only covers the release directory, unit, and environment.
