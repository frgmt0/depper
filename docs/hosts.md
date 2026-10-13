# Preparing a host

depper needs a Linux host that you can reach over SSH, with a systemd user manager for the account that runs the service. There are two ways to get there:

- [`depper host init`](#automated-setup-with-depper-host-init) prepares a fresh VPS from your workstation in one command.
- [Manual setup](#manual-setup) describes the same result step by step, for hosts you configure yourself or distributions that `host init` does not support.

Both end with [`depper doctor`](#verify-with-depper-doctor).

## Automated setup with `depper host init`

```sh
depper init                          # once per repository
depper host init root@203.0.113.10   # prepare the host and write the connection into depper.toml
depper deploy
```

`depper host init ADMIN@HOST [REPO]` connects as an administrator account (`ADMIN`, root or a user with passwordless sudo), prepares the host, and writes the connection details into the repository's `depper.toml`. `REPO` defaults to the current directory. The repository must already contain a valid `depper.toml`; run `depper init` first. The form `depper REPO host init ADMIN@HOST` is accepted as well.

### What it does

In order:

1. **Pin the host key.** Fetches the host's SSH keys with `ssh-keyscan`, shows their fingerprints, and after you confirm writes them to a repository-local known_hosts file. See [Host key confirmation](#host-key-confirmation-and-known_hosts).
2. **Read facts.** Over one SSH session as the administrator, reads the operating system, the privilege level, which packages and commands exist, whether the deploy user exists, and the state of sshd, the firewall, automatic updates, and Tailscale. This step changes nothing.
3. **Build and show the plan.** Every step below is checked first and listed as a change or as already satisfied. See [The plan](#the-plan-confirmation-and-dry-run).
4. **Apply the changes as root**, in this order (a step that is already satisfied is skipped):
   1. Install packages: `rsync`, `curl`, `ca-certificates`, and `coreutils` (for `sha256sum`), plus the packages for the container engine, firewall, and automatic updates you asked for. Debian and Ubuntu use `apt-get`, Fedora and RHEL use `dnf`.
   2. Install `cloudflared` from Cloudflare's package repository (`--cloudflared`).
   3. Install Tailscale from Tailscale's package repository (`--tailscale-authkey-env`).
   4. Create the deploy user: no password, home `/home/USER`, login shell `/bin/bash` (`/bin/sh` if bash is missing), no `sudo`.
   5. Append your public key to `~USER/.ssh/authorized_keys` (directory mode 700, file mode 600, owned by the deploy user). Existing keys are kept and the key is not added twice. depper refuses to write through a symbolic link.
   6. Enable lingering for the deploy user (`loginctl enable-linger`).
   7. Start the deploy user's systemd user manager and wait up to 30 seconds until `systemctl --user` works.
   8. Container engine setup: for podman, a 65536-id range in `/etc/subuid` and `/etc/subgid` if the user has none; for docker, membership in the `docker` group and `docker.service` enabled.
   9. Enable the firewall (`--firewall`).
   10. Enable automatic security updates (`--auto-updates`).
   11. Join the tailnet (`--tailscale-authkey-env`).
   12. Harden sshd (`--harden-ssh`), in its own guarded phase. See [Hardening options](#hardening-options).
   13. Write the host record `/etc/depper/host.json`.
5. **Verify.** Runs the checks of [`depper doctor`](#verify-with-depper-doctor) as the deploy user with the pinned key.
6. **Write `depper.toml`.** Sets `host`, `user`, `port` (only when it is not 22), `known_hosts`, and, when you passed `--key`, `identity_file`. See [What is written to depper.toml](#what-is-written-to-deppertoml).

The command finishes by printing `Next: depper deploy`, with `--target NAME` added when you used a target. If the final doctor run reports a failure, the command exits with status 1 after the host has been prepared and the configuration written, and says so.

### Requirements

On your machine: `ssh`, `ssh-keyscan` (both ship with the OpenSSH client), and a public key. Without `--key`, depper uses the first of `~/.ssh/id_ed25519.pub`, `~/.ssh/id_ecdsa.pub`, and `~/.ssh/id_rsa.pub`; if none exists, create one with `ssh-keygen -t ed25519`.

On the host:

- A Debian/Ubuntu family system (`apt`) or a Fedora/RHEL family system (`dnf`). depper reads `ID` and `ID_LIKE` from `/etc/os-release`: `debian`, `ubuntu`, `fedora`, `rhel`, `centos`, `rocky`, and `almalinux` are recognised, as are derivatives that list one of `debian`, `ubuntu`, `fedora`, `rhel`, or `centos` in `ID_LIKE`. Other systems are refused before anything changes.
- systemd as the init system (`/run/systemd/system` must exist) and `runuser` from util-linux.
- An administrator login that is root, or a user for which `sudo -n` works. depper connects with `BatchMode=yes` and cannot type a password, so a user whose `sudo` asks for one is refused, with a message showing the sudoers line that would allow it. A non-root administrator needs an SSH key; password login is not supported.
- Network access from the host to its package repositories.

The deploy user must not be `root`, must differ from the administrator, and must match `^[a-z_][a-z0-9_-]{0,31}$`. An existing account is reused and left unchanged, except that a missing key, lingering, or user manager is added. An account with uid 0 is refused. depper warns when an existing account is in the `sudo`, `wheel`, `admin`, or `root` group, or in the `docker` group without `--container docker`.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port N` | `22` | SSH port of the host. Recorded in `depper.toml` only when it is not 22. |
| `--identity FILE` | ssh's own choice | Private key for the administrator login. Not recorded in `depper.toml`. |
| `--user NAME` | `deploy` | Deploy account to create. |
| `--key FILE.pub` | first of `~/.ssh/id_ed25519.pub`, `id_ecdsa.pub`, `id_rsa.pub` | Public key to authorize for the deploy user. When given, the private key next to it (the same path without `.pub`), if it exists, is written to `identity_file`. |
| `--container ENGINE` | the engine in `depper.toml` | `podman`, `docker`, or `none`. See [Container engine](#container-engine). |
| `--cloudflared` | off | Install `cloudflared` from Cloudflare's package repository. |
| `--harden-ssh` | off | Disable password and keyboard-interactive SSH login. Root key login stays allowed. |
| `--firewall PORTS` | off | Enable `ufw` (Debian family) or `firewalld` (Fedora family) allowing these TCP ports, for example `22,80,443`. The SSH port is always included. |
| `--auto-updates` | off | Enable automatic security updates (`unattended-upgrades` or `dnf-automatic`). |
| `--tailscale-authkey-env NAME` | off | Install Tailscale and join the tailnet with the auth key held in the environment variable `NAME`. |
| `--known-hosts FILE` | `REPO/.depper/known_hosts` | known_hosts file in which to pin the host key. |
| `--write-config` | `true` | Write the connection into `depper.toml`. |
| `--no-write-config` | off | Do not modify `depper.toml`. The file must still exist and be valid. |
| `--dry-run` | off | Show the host keys and the plan; change nothing. |
| `-y`, `--yes` | off | Do not ask for confirmation (host key and plan). |

Global flags: `--target NAME` writes the connection to `[targets.NAME]` instead of `[deploy]` and selects the target whose `[container]` settings decide the engine; `--json` writes the result as JSON to stdout and progress to stderr.

Paths given to `--identity`, `--key`, and `--known-hosts` may not contain whitespace or control characters. The port, user, host, and flag values are validated before the host is contacted.

### Host key confirmation and known_hosts

The first step trusts a host key, so it needs your attention. depper runs `ssh-keyscan -T 15 -p PORT HOST`, prints each key type with its `SHA256:` fingerprint, and asks:

```
The host 203.0.113.10 presents these SSH host keys:
  ssh-ed25519          SHA256:...
Compare them with the fingerprints from your provider's console (or `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the host).
Trust these host keys and write them to /path/to/repo/.depper/known_hosts? [y/N]
```

`ssh-keyscan` returns whatever the network returns, so compare the fingerprints with your provider's console before you answer yes. With `--yes` there is no prompt and the keys are pinned as presented; use it only when you have verified the path to the host, or when the fingerprint check happens elsewhere.

Rules:

- The file defaults to `REPO/.depper/known_hosts`; `--known-hosts FILE` chooses another. It is created with mode 600, written atomically, and existing lines are kept. Entries are named `HOST` for port 22 and `[HOST]:PORT` for other ports, which is how ssh looks them up.
- If the file already holds a key for the host and it matches the presented key, depper reports "already pinned" and does not ask.
- If the file holds a different key, or only keys of other types, depper stops with `HOST KEY MISMATCH`, prints both sets of fingerprints, and changes nothing. Verify the new fingerprint through a trusted channel (a reinstalled host has new keys; an interception looks the same), then remove the old entry by hand and run the command again. A key marked `@revoked` also stops it.
- With `--dry-run` the keys are shown but not written. The facts are read over a connection that trusts those keys for this run only, using a temporary file that is deleted afterwards.

`depper.toml` receives `known_hosts = ".depper/known_hosts"` (a path relative to the repository when the file is inside it, `~/...` under your home directory, otherwise absolute), so every later depper command uses `StrictHostKeyChecking=yes` against that file only.

Host keys are public. Commit `.depper/known_hosts`, or paste its contents into the `DEPPER_KNOWN_HOSTS` secret that CI uses (see [CI/CD](ci.md#using-the-known_hosts-written-by-host-init)). The command prints a reminder to do one or the other. In `source = "worktree"` mode the file is uploaded with the release like any other file that is not excluded; it contains only public keys.

### The plan, confirmation, and dry run

After the facts are read, depper prints the plan: changes are numbered, steps that are already satisfied are listed with `ok`, and warnings follow.

```
203.0.113.10: Ubuntu 24.04 LTS (x86_64), admin root (root)
  1. Pin the host key in .depper/known_hosts: write 1 key(s) for 203.0.113.10 (mode 600)
  2. Create the deploy user deploy: no password, home /home/deploy, shell /bin/bash (or /bin/sh), no sudo
  ...
  ok  Install packages: already installed: rsync curl ca-certificates coreutils
```

(The output is abbreviated here and its wording may differ.) Then:

- **Interactive terminal.** depper asks for the host key and, if the host needs changes, once more for `Apply these changes to HOST? [y/N]`. Anything other than `y` or `yes` cancels with "nothing was changed on the host".
- **No terminal.** Without `--yes`, depper refuses to run (`stdin is not a terminal`) rather than guess. Inspect the plan with `--dry-run`, then run again with `--yes`.
- **`--dry-run`.** Prints the host keys and the plan and stops. It connects to the host as the administrator and only reads; it writes nothing on the host or in the repository.
- **`--yes`.** Skips both questions. The plan is still printed.

Everything that can be validated locally is checked before the host is touched: the options, the public key, and whether `depper.toml` parses and still resolves after the edit.

### What is written to depper.toml

`host init` makes a minimal text edit. A key that is present has its value replaced in place (indentation and a trailing comment are kept); a missing key is inserted right after the table header. Comments and all other lines are untouched. If the target table does not exist, `[targets.NAME]` is appended.

| Key | Value |
| --- | --- |
| `host` | The address from `ADMIN@HOST`, without a user. |
| `user` | The deploy user. |
| `port` | The SSH port, only when it is not 22. |
| `identity_file` | The private key next to `--key`, only when `--key` is given and that file exists. |
| `known_hosts` | The known_hosts file. |

The edit is validated before the host is touched and again after it is written. If the written file fails validation the original is restored. If a value is an array, an inline table, or a multi-line string, or if the edit would break another target, depper stops and tells you what to set by hand. A `[deploy]` table is required in the base file; `[targets.NAME]` is created if missing. The file is written after the host was prepared and the doctor run finished. If applying the plan fails, `depper.toml` is left as it was; if only the final doctor run reports a failure, the file is still written. `--no-write-config` skips the write.

### Running it again

Every step is checked before it is applied, so a second run against a prepared host changes nothing: it reports "Nothing to change", `applied` is empty in `--json`, and `depper.toml` and the host record are not rewritten. Re-run it to add an option later, for example `depper host init root@203.0.113.10 --firewall 22,80,443` after the first run; options you do not repeat are not undone (see [host.json](#etcdepperhostjson)). It also repairs a host in which lingering was turned off or a key was removed.

If a run stops part-way, the steps that completed stay applied and are recorded in `host.json`; run the command again after fixing the cause.

### Hardening options

All hardening is opt-in. Nothing in this section happens unless you pass the flag. depper never edits `/etc/ssh/sshd_config`; it adds files of its own, so each change can be undone by removing what depper added.

#### `--harden-ssh`

Writes `/etc/ssh/sshd_config.d/10-depper.conf` (mode 644):

```
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
```

Root login with a key stays allowed because the administrator may be root. The file name sorts before the drop-ins that cloud images ship (Ubuntu's `50-cloud-init.conf`), and sshd keeps the first value it reads for each option, so depper's settings win over those. Only a line that sorts earlier, or a line in the main `sshd_config` before its `Include`, can override them.

Safety checks, all before anything is locked down:

- **Refusal at planning time.** depper refuses to continue, changing nothing, when:
  - `sshd` is not installed;
  - the main `/etc/ssh/sshd_config` has no `Include /etc/ssh/sshd_config.d/*.conf` line, because the drop-in would be ignored (add the line yourself, or drop the flag);
  - the current sshd configuration does not pass `sshd -T`;
  - `PasswordAuthentication`, `KbdInteractiveAuthentication`, `ChallengeResponseAuthentication`, or `PermitRootLogin` is set to `yes` in the main config before its `Include`, or in a drop-in that sorts before `10-depper.conf`. The error names those files, for example `sshd_config.d/01-local.conf`. Change or remove those lines first.
- **Validation after writing.** depper writes the file, runs `sshd -t`, and checks the effective settings with `sshd -T`. If sshd rejects the configuration, or an option still has a weaker value, the file is removed and the command fails.
- **Key login proof.** Before sshd is reloaded, depper opens a new SSH connection as the deploy user with its key and runs a test script. If that fails, the file is removed and sshd is not reloaded.
- **Reload and re-check.** depper reloads `ssh.service` or `sshd.service` (whichever is active), then opens new connections as the deploy user and as the administrator. If either fails, it removes the file and reloads again. If that rollback itself fails, the error tells you to remove the file and reload sshd from the provider console.

Undo:

```sh
sudo rm /etc/ssh/sshd_config.d/10-depper.conf
sudo systemctl reload ssh        # or sshd
```

Afterwards `depper host status` reports `ssh hardening` as a failure, because the record still says depper applied it. Edit `/etc/depper/host.json` and set `"ssh": false` under `hardening` to clear that.

#### `--firewall PORTS`

Enables a host firewall allowing the listed TCP ports. The SSH port you connect on (`--port`) is always added, so the port in use cannot be closed by the list you give. For example `--firewall 80,443` allows 22, 80, and 443.

- Debian and Ubuntu: installs `ufw`, runs `ufw allow N/tcp` for each port that is not allowed yet, then `ufw --force enable` if ufw is inactive. ufw's default policy applies to everything else.
- Fedora and RHEL: installs `firewalld`. If it is running, depper adds each missing port with `firewall-cmd --permanent --add-port` and reloads. Otherwise it adds the ports offline and enables `firewalld.service`. Port 22 counts as allowed when the `ssh` service is allowed.
- Existing rules are never removed. The packages are listed in `host.json`.

depper does not check other listening services: a port that you do not list is closed to new connections once the firewall is enabled. Do not enable a firewall on a host whose other services you cannot account for.

Undo: `sudo ufw disable`, or `sudo ufw delete allow 80/tcp` for one port; on firewalld, `sudo firewall-cmd --permanent --remove-port=80/tcp && sudo firewall-cmd --reload`, or `sudo systemctl disable --now firewalld`. Then edit `hardening.firewall` in `host.json` to match.

#### `--auto-updates`

Enables automatic security updates.

- Debian and Ubuntu: installs `unattended-upgrades` and writes `/etc/apt/apt.conf.d/20auto-upgrades` with `APT::Periodic::Update-Package-Lists "1";` and `APT::Periodic::Unattended-Upgrade "1";`.
- Fedora and RHEL: installs `dnf-automatic`, sets `upgrade_type = security` and `apply_updates = yes` in `/etc/dnf/automatic.conf` (editing those lines in place, or creating the file), and enables `dnf-automatic-install.timer` (dnf 4) or `dnf5-automatic.timer` (dnf 5).

Automatic updates can restart services and occasionally change behaviour. Undo on Debian: set both values in `20auto-upgrades` to `"0"` or remove the file. On Fedora: `sudo systemctl disable --now dnf-automatic-install.timer` (or `dnf5-automatic.timer`) and restore the two settings in `automatic.conf` by hand; depper keeps no backup of that file.

#### Tailscale

`--tailscale-authkey-env NAME` installs Tailscale from Tailscale's official package repository (`pkgs.tailscale.com`) and runs `tailscale up` with the auth key held in the environment variable `NAME` on your machine:

```sh
export TS_AUTHKEY=tskey-auth-...        # a one-time, short-lived key from the Tailscale admin console
depper host init root@203.0.113.10 --tailscale-authkey-env TS_AUTHKEY
```

Key handling:

- You pass the name of a variable, never the key itself, so it does not appear in shell history or process lists on your machine. The command fails if the variable is unset, empty, or holds whitespace or non-ASCII characters.
- The key is sent to the host inside the script on SSH's standard input, written to a mode 600 temporary file, handed to `tailscale up --authkey=file:...`, and deleted right after. It is never part of a command line on the host, printed, or recorded. In `--json` output it is shown as `<redacted>`. `host.json` records only that Tailscale was set up.
- Prefer a single-use key with a short expiry, and revoke it in the admin console afterwards. A tailnet that is already up is left alone and the key is not used.

depper does not change `host` in `depper.toml` to the tailnet address. To deploy over the tailnet, set `host` yourself. Tailscale gives you private reachability; it does not replace SSH authentication, and a CI runner must be able to reach the tailnet address.

Undo: `sudo tailscale down`, `sudo tailscale logout`, remove the machine in the admin console, and uninstall the `tailscale` package.

#### `--cloudflared`

Installs `cloudflared` from Cloudflare's package repository (`pkg.cloudflare.com`; an apt source with a signed keyring, or a yum repo file). It installs the binary only: it does not log in or create a tunnel. See [Cloudflare Tunnel](tunnels.md). Undo: uninstall the package and remove `/etc/apt/sources.list.d/cloudflared.list` and `/usr/share/keyrings/cloudflare-main.gpg` (Debian family) or `/etc/yum.repos.d/cloudflared.repo` (Fedora family).

### Container engine

`--container` takes `podman`, `docker`, or `none`.

Without the flag, `host init` installs the engine that the resolved `depper.toml` uses: `[container].engine`, which is `docker` when `[container]` has no `engine` key, and nothing when there is no `[container]`. `depper init` writes `engine = "podman"` for container projects, so by default a detected container project gets rootless podman. `--container none` with a `[container]` table prints a warning, since the service would then have no engine.

- **podman:** `podman` plus its rootless helpers (`uidmap` and `slirp4netns` on Debian and Ubuntu; `shadow-utils` and `passt` on Fedora; `shadow-utils` and `slirp4netns` on other RHEL-family systems), and a 65536-id range in `/etc/subuid` and `/etc/subgid` for the deploy user if it has none.
- **docker:** the distribution package (`docker.io` on Debian and Ubuntu, `moby-engine` on Fedora), the deploy user added to the `docker` group, and `docker.service` enabled. RHEL, CentOS, Rocky, and AlmaLinux have no distribution docker package, so `host init` refuses; install `docker-ce` from Docker's repository yourself, or use podman. depper prints this warning every time it adds the user to the group: membership in the `docker` group is root-equivalent on the host. Prefer rootless podman unless you need docker. See [Containers](containers.md#host-requirements).

Undo: remove the packages, and for docker run `sudo gpasswd -d deploy docker`.

### /etc/depper/host.json

After a run, depper writes `/etc/depper/host.json` as root (directory mode 755, file mode 644, written atomically). It records what depper set up, so `depper host status` can tell what should be present, and it never contains a secret.

```json
{
  "depper_version": "0.2.0",
  "created_at": "2026-10-13T09:00:00Z",
  "updated_at": "2026-10-13T09:00:00Z",
  "deploy_user": "deploy",
  "container": "podman",
  "cloudflared": false,
  "steps": ["authorized-key", "firewall", "harden-ssh", "linger", "packages", "subids", "user", "user-manager"],
  "packages": ["podman", "slirp4netns", "ufw", "uidmap"],
  "hardening": {
    "ssh": true,
    "firewall": [22, 80, 443],
    "auto_updates": false,
    "tailscale": false
  }
}
```

- `steps` lists the step ids depper applied, and `packages` the packages it installed (not packages that were already there). Use it to see what to remove when you decommission the host.
- The record is merged across runs for the same deploy user: steps and packages accumulate, and a hardening option stays recorded after a later run that omits the flag. It records only features whose steps completed; a step that failed is not claimed.
- The file is informational. depper does not read it to decide what to change; it reads the live host every time.

### depper host status

```sh
depper host status [REPO]
depper --target production host status
```

Connects as the deploy user from `depper.toml` (not as the administrator, so no root rights are needed), reads `/etc/depper/host.json` and the live state, and changes nothing. Each check prints `PASS`, `WARN`, `FAIL`, or `INFO`:

| Check | Result |
| --- | --- |
| `record` | Pass when `host.json` is readable. Warn when it is missing (the host was not prepared with `host init`) or not readable by the user. |
| `user` | Pass when the connection works as the user in the record. Warn when you are connected as another user. |
| `linger` | Fail when lingering is off for the user. |
| `systemd` | Fail when `systemctl --user` cannot reach the user manager. |
| `container engine` | Pass when the engine from `[container]` (or the record) is installed; fail when it is not; info when none is configured. |
| `cloudflared` | Shown only with `[tunnel]` or a recorded install. Pass when installed, fail when not. |
| `ssh hardening`, `firewall`, `auto updates`, `tailscale` | Pass when the feature is live. Fail when the record says depper enabled it and it is no longer in place. Info (`not enabled (opt-in)`) when it was never enabled. |

With human output, `host status` first prints the depper version and timestamps from the record and the packages depper installed. The exit status is 1 if any check fails, otherwise 0. `--json` prints `{"host", "user", "target", "record", "checks": [{"name", "status", "detail"}, ...]}`, where `status` is `pass`, `warn`, `fail`, or `info` and `record` is the contents of `host.json` or `null`.

`host status` complements `depper doctor`, which checks what a deployment needs (rsync, `sha256sum`, disk space, locks); `host status` checks how the host was prepared.

### Troubleshooting

#### Another user's XDG variables in /etc/environment

Some images set `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, or similar in `/etc/environment` for their own default user (for example `/run/user/1001` and `/home/runner/.config`). `pam_env` applies that file to every login and systemd applies it to every user manager, so the deploy user's manager and rootless podman then try to use another user's directories and fail: units cannot start, or podman refuses a runtime directory it does not own.

depper's own scripts ignore an inherited `XDG_RUNTIME_DIR` or `XDG_*_HOME` that the deploy user does not own, but the units run inside the user manager's environment, which depper does not control. To fix the host, remove the lines that name another user's directories from `/etc/environment` (as root):

```sh
grep '^XDG_' /etc/environment                   # see what is set
sudo sed -i -E '/^XDG_[A-Z_]+=/d' /etc/environment
```

This deletes every `XDG_` line, so first check that none of them is wanted. Managers started afterwards pick up the change; for a manager that is already running, restart it with `sudo systemctl restart user@$(id -u deploy).service` (this stops the deploy user's services until they start again) or reboot. A fresh VPS does not have these lines.

### Support status

What follows is the extent of real-host verification; the rest is covered only by unit tests against a fake host, which do not show that a command behaves as expected on a real system.

Verified on a real Ubuntu 24.04 VM (a GitHub-hosted runner, acting as its own deploy target over SSH to `127.0.0.1`, with `root` as administrator; see [Development](development.md#end-to-end-tests)):

- installing `openssh-server`, and sshd reload with `ssh.socket` activation
- apt packages, `useradd`, lingering, subordinate ids, and the user manager
- `--harden-ssh` and `--firewall` (ufw), including the second run changing nothing
- `host status`
- `ssh -W` health checks
- rsync transfers and deploys
- podman in build mode, image mode, and compose mode (`podman-compose`)
- secrets push, rollback, and unlock

Not verified on real hosts:

- Fedora, RHEL, CentOS, Rocky, and AlmaLinux (`dnf`, `firewalld`, `dnf-automatic`)
- Tailscale installation and `tailscale up`
- `cloudflared` installation
- the docker engine path (group membership and `docker.service`)
- a non-root administrator using `sudo`
- SSH ports other than 22
- IPv6 hosts

Treat those paths as untested. Run `--dry-run` first and read the plan.

## Manual setup

Use this when `host init` does not support your distribution, when you do not want depper to change the host, or when an administrator prepares hosts for you. Package names and commands are for Debian and Ubuntu; adapt them for other distributions. The end state is what `host init` produces.

### Requirements

- Linux with systemd. The service runs under `systemctl --user`, so `systemd` must be the init system and the account needs a user manager.
- A POSIX `sh` as the login shell's `sh -s` target, and GNU-compatible coreutils. depper uses `sha256sum`, `mktemp`, `base64 -d`, `readlink`, `ln -s`, `mv -T`, `find`, `head`, and `df`.
- `rsync`.
- `curl` is optional. It is used only if SSH TCP forwarding is refused for [URL health checks](health.md).
- Whatever your application needs: a language runtime, or docker/podman for [container deployments](containers.md).
- A remote `$HOME` (or a `[deploy].path` that you set) made only of letters, digits, and `. _ @ + / -`. The default base directory is `$HOME/.local/share/depper/<name>`, and depper uses base paths unquoted in rsync destinations and unit files. If the account's home directory contains other characters, such as a space, set `[deploy].path` to an absolute path that meets this rule; depper otherwise stops with an error that says so.

depper never uses root and never writes outside the account's home directory, apart from a custom `[deploy].path` that you choose. Only `host init` uses root, and only on the host you point it at.

### Create a dedicated account

Use an unprivileged account for deployments. It needs write access to its own application directory and a user systemd manager, nothing more.

```sh
sudo adduser --disabled-password --gecos '' deploy
sudo apt update
sudo apt install -y openssh-server rsync
```

Do not add the account to `sudo`. If you use docker, read the note on group membership in [Containers](containers.md#host-requirements) first.

### Enable lingering

User services normally stop when the last login session ends and do not start at boot. Lingering keeps the user manager running:

```sh
sudo loginctl enable-linger deploy
loginctl show-user deploy -p Linger        # Linger=yes
```

This is usually the only step that needs an administrator. Without it, services stop when your SSH session closes, and `depper doctor` reports a warning.

### SSH access

depper runs `ssh` and `rsync` as subprocesses, so your `~/.ssh/config`, agent, `ProxyJump`, and `ControlMaster` settings all apply. Connections use `BatchMode=yes`: there are no password or host key prompts. Local key and known_hosts file paths (`identity_file`, `known_hosts`, and the CI variables `DEPPER_SSH_KEY_FILE` and `DEPPER_KNOWN_HOSTS_FILE`) may not contain whitespace.

#### Generate a deploy key

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

#### Choose how to name the host

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

#### Host keys and known_hosts

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

With `known_hosts` set, depper passes `UserKnownHostsFile=<file>` and `StrictHostKeyChecking=yes`: only the keys in that file are accepted. The file entry must match the `host` value (an IP address entry does not match a DNS name). `ssh-keyscan` output is not authenticated by itself; check the fingerprint through a trusted channel. `host init` does the same thing, with a confirmation prompt.

### systemd user units

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

### Exposing a port

depper does not configure a firewall during a deployment (`host init --firewall` can enable one when it prepares the host). A service that binds `0.0.0.0` or a published container port is reachable from whatever the host's network allows. To make a service reachable from outside:

1. Bind it to the address you intend (`127.0.0.1` for local-only, or all interfaces).
2. Open the port in the host firewall (for example `sudo ufw allow 8080/tcp`) and in the provider's network firewall if there is one.
3. Record the address in `[endpoint]` so `depper status` shows it.

Prefer binding to `127.0.0.1` and putting TLS and authentication in front of the service (a reverse proxy, Tailscale, or a [Cloudflare Tunnel](tunnels.md)). Health checks run on the host, so they work with a loopback-only service.

## Verify with depper doctor

```sh
depper doctor
```

`doctor` runs one SSH session and reports `pass`, `warn`, or `fail` for each item. `host init` runs the same checks at the end of its run.

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

- Keep the deploy account narrow. Anyone who can use the deploy key can run arbitrary commands as that account, because `[service].command`, `setup`, and health commands are shell commands. `host init` creates the account without a password and without `sudo`.
- `host init` runs commands as root on the host you name, and with `--yes` it pins the host key without asking. Verify the fingerprint, and run `--dry-run` first on hosts you care about.
- Do not put secrets in `depper.toml` or in the repository. Use a [dotenv file](secrets.md).
- Secrets on the host are protected by file permissions (mode 600 inside a mode 700 directory) and are readable by the service account and by root.
- Take backups of application data yourself; depper's rollback only covers the release directory, unit, and environment.
