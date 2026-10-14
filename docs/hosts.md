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

1. **Pin the host key.** Fetches the host's SSH keys with `ssh-keyscan`, shows their fingerprints, and after you confirm (or when they match `--host-key-fingerprint`) writes them to a repository-local known_hosts file. See [Host key confirmation](#host-key-confirmation-and-known_hosts).
2. **Read facts.** Over one SSH session as the administrator, reads the operating system, the privilege level, which packages and commands exist, whether the deploy user exists, and the state of sshd, the firewall, automatic updates, and Tailscale. This step changes nothing.
3. **Build and show the plan.** Every step below is checked first and listed as a change or as already satisfied. See [The plan](#the-plan-confirmation-and-dry-run).
4. **Apply the changes as root**, in this order (a step that is already satisfied is skipped):
   1. Install packages: `rsync`, `curl`, `ca-certificates`, and `coreutils` (for `sha256sum`), plus the packages for the container engine, firewall, and automatic updates you asked for. Debian and Ubuntu use `apt-get`, Fedora and RHEL use `dnf`, Arch Linux uses `pacman -S --needed --noconfirm` from the sync database as it is (see [Arch Linux](#arch-linux)).
   2. Install `cloudflared` from Cloudflare's package repository (`--cloudflared`).
   3. Install Tailscale from Tailscale's package repository (`--tailscale-authkey-env`).
   4. Create the deploy user: no password, home `/home/USER`, login shell `/bin/bash` (`/bin/sh` if bash is missing), no `sudo`.
   5. Append your public key to `~USER/.ssh/authorized_keys` (directory mode 700, file mode 600, owned by the deploy user). Existing keys are kept and the key is not added twice. Commented lines are ignored. A line that holds the key but has options (`command=`, `from=`, `restrict`, `no-pty`, ...) counts as present but restricted: depper warns and leaves it alone, and appends an unrestricted line only when you passed `--key` explicitly. depper refuses to write through a symbolic link.
   6. Enable lingering for the deploy user (`loginctl enable-linger`).
   7. Start the deploy user's systemd user manager and wait up to 30 seconds until `systemctl --user` works.
   8. Container engine setup: for podman, a 65536-id range in `/etc/subuid` and `/etc/subgid` if the user has none; for docker, membership in the `docker` group and `docker.service` enabled.
   9. Enable automatic security updates (`--auto-updates`).
   10. Join the tailnet (`--tailscale-authkey-env`).
   11. Enable the firewall (`--firewall`), in its own guarded phase. See [Guarded changes](#guarded-changes-automatic-revert).
   12. Harden sshd (`--harden-ssh`), in its own guarded phase.
5. **Verify.** Runs the checks of [`depper doctor`](#verify-with-depper-doctor) as the deploy user with the pinned key.
6. **Write `depper.toml`**, only when the doctor run passed. Sets `host`, `user`, `port`, `known_hosts`, and `identity_file` where needed. See [What is written to depper.toml](#what-is-written-to-deppertoml).
7. **Write the host record** `/etc/depper/host.json`, after the configuration. It is also written after a failed doctor run or a failed apply, listing the steps that did happen.

The command finishes by printing `Next: depper deploy`, with `--target NAME` added when you used a target. If the final doctor run reports a failure, the command exits with status 1 after the host has been prepared, says so, does not change `depper.toml`, and prints the edit it would have made.

Every ssh connection `host init` opens (administrator and deploy user, a dry run, and the final doctor run) passes `-o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes`, so nothing from your machine is forwarded to the host. The connections other depper commands use are unchanged.

### Requirements

On your machine: `ssh`, `ssh-keyscan` (both ship with the OpenSSH client), and a public key. Without `--key`, depper uses the first of `~/.ssh/id_ed25519.pub`, `~/.ssh/id_ecdsa.pub`, and `~/.ssh/id_rsa.pub`; if none exists, create one with `ssh-keygen -t ed25519`.

On the host:

- A Debian/Ubuntu family system (`apt`) or a Fedora/RHEL family system (`dnf`). depper reads `ID` and `ID_LIKE` from `/etc/os-release`: `debian`, `ubuntu`, `fedora`, `rhel`, `centos`, `rocky`, and `almalinux` are recognised, as are derivatives that list one of `debian`, `ubuntu`, `fedora`, `rhel`, or `centos` in `ID_LIKE`. Other systems are refused before anything changes. Amazon Linux 2023 and later (`ID=amzn`) is handled as a dnf system: podman is refused there (use docker), and Tailscale comes from its Amazon Linux repository. Amazon Linux 2 is refused before anything changes. Arch Linux (`ID=arch`) and derivatives that list `arch` in `ID_LIKE` (Manjaro, EndeavourOS, CachyOS, Omarchy, ...) are handled as pacman systems; see [Arch Linux](#arch-linux). The plan names the detected `ID` and the family it is handled as.
- systemd as the init system (`/run/systemd/system` must exist) and `runuser` from util-linux. A `--harden-ssh` or `--firewall` step that actually changes something also needs `systemd-run` and systemd 236 or later (for `--collect`); otherwise host init refuses with a clear message (see [Guarded changes](#guarded-changes-automatic-revert)).
- An administrator login that is root, or a user for which `sudo -n` works. depper connects with `BatchMode=yes` and cannot type a password, so a user whose `sudo` asks for one (for example a `wheel` member under Arch's `%wheel ALL=(ALL:ALL) ALL`) is refused before anything changes. The message names both ways forward: connect as root with an SSH key, or temporarily allow passwordless sudo with the sudoers line it shows, and remove that line after host init. A non-root administrator needs an SSH key; password login is not supported.
- Network access from the host to its package repositories.

The deploy user must not be `root`, must differ from the administrator, and must match `^[a-z_][a-z0-9_-]{0,31}$`. An existing account is reused and left unchanged, except that a missing key, lingering, or user manager is added, and the two repairs for a half-created account described below. An account with uid 0 is refused. depper warns when an existing account is in the `sudo`, `wheel`, `admin`, or `root` group, or in the `docker` group without `--container docker`.

Half-created accounts:

- If the account's password field is exactly `!` or `!!` and sshd has `UsePAM no`, the account cannot log in with a key (sshd treats a locked account as unusable without PAM). depper plans `usermod -p '*'`, which disables password login without locking the account. A new account is created with the same `*` field.
- If the login shell is `nologin` or `false`, depper runs `usermod -s /bin/sh` only when `/etc/depper/host.json` says depper created that user. Otherwise it only warns and leaves the account alone.

### Arch Linux

Arch is a rolling release, so `host init` installs packages without ever touching the package database's freshness:

- It never runs `pacman -Sy` (refreshing the sync database without upgrading the system is a partial upgrade, which Arch does not support), and it never runs `pacman -Syu` for you.
- While reading facts it asks pacman which packages are installed (`pacman -Q`) and which the local sync database has (`pacman -Si`). It installs with `pacman -S --needed --noconfirm` only when every package it needs is in that database. If one is missing, or the sync database is empty, it refuses before changing anything and asks you to run `sudo pacman -Syu` yourself (and reboot after a kernel upgrade), then run host init again. If pacman still fails to download a package because the mirrors moved on, the step fails with the same advice.
- Packages: `rsync`, `curl`, `ca-certificates`, `coreutils`; for podman, `podman`, `shadow` (for `newuidmap`) and `passt` (or `slirp4netns` when that is installed, or the only one available); for docker, `docker`; for compose, `podman-compose` or `docker-compose` (version 2, which installs the `docker compose` CLI plugin); `cloudflared` and `tailscale` from the official repositories; `ufw` for `--firewall` when no firewall is running.
- `--auto-updates` is refused: unattended upgrades are not supported practice on Arch. Run `sudo pacman -Syu` regularly yourself.
- `--firewall` uses ufw when it is active, otherwise firewalld when it is running, otherwise it installs ufw. It refuses when `nftables.service`, `iptables.service`, or `ip6tables.service` is active with a non-empty ruleset (or one it cannot read) and neither ufw nor firewalld is active, because depper does not replace a hand-written ruleset; and when `firewalld.service` is enabled but not running, because it would start next to ufw at the next boot. Arch does not enable `ufw.service` when the package is installed, so depper enables it (`systemctl enable ufw.service`) as part of the guarded firewall step, and the revert disables it again.
- sshd runs as `sshd.service`. Arch's `99-archlinux.conf` and systemd's `20-systemd-userdb.conf` (which sets `AuthorizedKeysCommand` for `userdbctl`) sort after `10-depper.conf` and set nothing that `--harden-ssh` changes, so they are not conflicts. `AuthorizedKeysCommand` does not replace `AuthorizedKeysFile`: the deploy user's `~/.ssh/authorized_keys` keeps working.
- Arch's `useradd` may add subordinate id ranges itself, depending on `/etc/login.defs`; depper checks `/etc/subuid` and `/etc/subgid` and adds a range with `usermod --add-subuids` / `--add-subgids` only when the user has none.
- `sudo` is not part of Arch's base install. Connect as root with an SSH key (sshd's default `PermitRootLogin prohibit-password` allows it), or as a user with passwordless sudo.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port N` | `22` | SSH port of the host. Recorded in `depper.toml` when it is not 22, or when the key already exists. |
| `--identity FILE` | ssh's own choice | Private key for the administrator login. Not recorded in `depper.toml`. |
| `--user NAME` | `deploy` | Deploy account to create. |
| `--key FILE.pub` | first of `~/.ssh/id_ed25519.pub`, `id_ecdsa.pub`, `id_rsa.pub` | Public key to authorize for the deploy user. When given, the private key next to it (the same path without `.pub`), if it exists, is written to `identity_file`. |
| `--container ENGINE` | the engine in `depper.toml` | `podman`, `docker`, or `none`. See [Container engine](#container-engine). |
| `--cloudflared` | off | Install `cloudflared` from Cloudflare's package repository (on Arch, from the official repositories). |
| `--harden-ssh` | off | Disable password and keyboard-interactive SSH login, and root password login (a stricter `PermitRootLogin` is never weakened). Guarded: undone automatically unless new logins succeed. |
| `--firewall PORTS` | off | Enable `ufw` (Debian family; Arch unless firewalld is running) or `firewalld` (Fedora family) allowing these TCP ports, for example `80,443`. The `--port` value and every port sshd listens on are always included. Guarded: undone automatically unless new logins succeed. |
| `--auto-updates` | off | Enable automatic security updates (`unattended-upgrades` or `dnf-automatic`). Refused on Arch Linux. |
| `--tailscale-authkey-env NAME` | off | Install Tailscale and join the tailnet with the auth key held in the environment variable `NAME`. |
| `--known-hosts FILE` | `REPO/.depper/known_hosts` | known_hosts file in which to pin the host key. |
| `--host-key-fingerprint SHA256:...` | none | Expected host key fingerprint, as `ssh-keygen -lf` prints it. Repeatable. Only scanned keys that match a given fingerprint are pinned, without a prompt; if none matches, host init refuses. |
| `--trust-on-first-use` | off | With `--yes`, pin whatever keys the host presents, without verification. |
| `--write-config` | `true` | Write the connection into `depper.toml`. |
| `--no-write-config` | off | Do not modify `depper.toml`. The file must still exist and be valid. |
| `--dry-run` | off | Show the host keys and the plan; change nothing. |
| `-y`, `--yes` | off | Do not ask for confirmation of the plan. The host key is pinned only if it is verified: see [Host key confirmation](#host-key-confirmation-and-known_hosts). |

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

`ssh-keyscan` returns whatever the network returns, so compare the fingerprints with your provider's console before you answer yes.

To skip the prompt safely, pass the fingerprint you verified. Get it from the provider console, or on the host:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
depper host init root@203.0.113.10 --host-key-fingerprint SHA256:... --yes
```

`--host-key-fingerprint` can be given more than once. Only the scanned keys that match one of the fingerprints are pinned (host init prints how many); the others are ignored. If none matches, host init stops and changes nothing.

Without a fingerprint, `--yes` does not confirm an unverified key: when no key for the host is pinned yet, host init refuses and asks for `--host-key-fingerprint` or `--trust-on-first-use`. `--trust-on-first-use` pins whatever the host presents now; use it only when you have verified the path to the host elsewhere. If the keys are already pinned, `--yes` needs neither. In automation, pass one of the two.

Rules:

- The file defaults to `REPO/.depper/known_hosts`; `--known-hosts FILE` chooses another. It is created with mode 600, written atomically, and existing lines are kept. Entries are named `HOST` for port 22 and `[HOST]:PORT` for other ports, which is how ssh looks them up.
- If the file already holds a key for the host and it matches the presented key, depper reports "already pinned" and does not ask.
- If the file holds a different key, or only keys of other types, depper stops with `HOST KEY MISMATCH`, prints both sets of fingerprints, and changes nothing. Verify the new fingerprint through a trusted channel (a reinstalled host has new keys; an interception looks the same), then remove the old entry by hand and run the command again. A key marked `@revoked` also stops it.
- With `--dry-run` the keys are shown but not written. The facts are read over a connection that trusts those keys for this run only, using a temporary file that is deleted afterwards.

`depper.toml` receives `known_hosts = ".depper/known_hosts"` (a path relative to the repository when the file is inside it, `~/...` under your home directory, otherwise absolute), so every later depper command uses `StrictHostKeyChecking=yes` against that file only.

Host keys are public. Commit `.depper/known_hosts`, or paste its contents into the `DEPPER_KNOWN_HOSTS` secret that CI uses (see [CI/CD](ci.md#using-the-known_hosts-written-by-host-init)). The command prints a reminder to do one or the other. `.depper/` is always excluded from the files depper uploads (see [Configuration](configuration.md#deploy)), so `.depper/known_hosts` is never shipped in a release.

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
- **No terminal.** Without `--yes`, depper refuses to run (`stdin is not a terminal`) rather than guess. Inspect the plan with `--dry-run`, then run again with `--yes` and a `--host-key-fingerprint`.
- **`--dry-run`.** Prints the host keys and the plan and stops. It connects to the host as the administrator and only reads; it writes nothing on the host or in the repository.
- **`--yes`.** Skips the plan question. The plan is still printed. It confirms the host key only together with `--host-key-fingerprint` or `--trust-on-first-use` (or a key that is already pinned).

Everything that can be validated locally is checked before the host is touched: the options, the public key, and whether `depper.toml` parses and still resolves after the edit.

### What is written to depper.toml

`host init` makes a minimal text edit. A key that is present has its value replaced in place (indentation and a trailing comment are kept); a missing key is inserted right after the table header. Comments and all other lines are untouched. If the target table does not exist, `[targets.NAME]` is appended.

| Key | Value |
| --- | --- |
| `host` | The address from `ADMIN@HOST`, without a user. |
| `user` | The deploy user. |
| `port` | The SSH port. Written when the key already exists or the port is not 22, and also when a target would otherwise inherit a different port from `[deploy]`. |
| `identity_file` | With `--key`: the private key next to it (the same path without `.pub`), if it exists. Without `--key`: set only when the edited table already sets `identity_file`, or a target inherits a different one; it is then the private key of the authorized public key if that exists locally, else host init refuses and suggests `--key`. |
| `known_hosts` | The known_hosts file. |

The edit is checked before the host is touched. depper resolves the edited file and requires that `host`, `user`, `port`, `identity_file`, and `known_hosts` equal the intended values, that nothing outside the edited keys changed, and that every other target resolves to the same connection as before. Otherwise host init aborts without writing anything and tells you what to set by hand (for example when an environment override or an inherited value would win). The editor tokenises the file line by line, so `"""` or `'''` inside a string or a comment is not taken for a multi-line delimiter. If a value is an array, an inline table, or a multi-line string, depper stops and tells you what to set by hand. A `[deploy]` table is required in the base file; `[targets.NAME]` is created if missing.

`depper.toml` is written only after `depper doctor` has passed on the prepared host. If the doctor run fails, host init prints the edit it would have made and leaves the file alone. It is also left alone if applying the plan fails. Just before writing, depper re-reads the file and aborts with `depper.toml changed while host init was running` if it differs from what it planned against (the host is already prepared; run the command again). The written file is validated again, and the original is restored if that fails. Writes follow symbolic links: if `depper.toml` or the known_hosts file is a link, the file it points to is replaced and the link is kept. `--no-write-config` skips the write.

### Running it again

Every step is checked before it is applied, so a second run against a prepared host changes nothing: it reports "Nothing to change", `applied` is empty in `--json`, and `depper.toml` and the host record are not rewritten. Re-run it to add an option later, for example `depper host init root@203.0.113.10 --firewall 22,80,443` after the first run; options you do not repeat are not undone (see [host.json](#etcdepperhostjson)). It also repairs a host in which lingering was turned off or a key was removed.

If a run stops part-way, the steps that completed stay applied and are recorded in `host.json`; run the command again after fixing the cause.

### Hardening options

All hardening is opt-in. Nothing in this section happens unless you pass the flag. depper never edits `/etc/ssh/sshd_config`; it adds files of its own, so each change can be undone by removing what depper added.

#### Guarded changes (automatic revert)

`--harden-ssh` and `--firewall` can lock everyone out of the host, so each runs as a guarded change. A guarded step that has something to change needs `systemd-run` and systemd 236 or later (`--collect`); without them host init refuses with a clear message, changing nothing. A step that is already satisfied needs neither.

Each guarded change keeps its state in `/var/lib/depper/guard/UNIT/` on the host, where `UNIT` is `depper-ssh-revert` or `depper-firewall-revert`: the script `revert.sh` and explicit marker files that say what to undo.

| Change | Markers |
|---|---|
| `--harden-ssh` | `had-prev` and `prev` (a drop-in existed; `prev` is its copy) or `had-none` (there was none) |
| `--firewall` | `was-inactive` (the firewall was off), `added-ports` (the TCP ports depper added, one per line), `zone` (the firewalld zone, recorded when the revert is armed; the step and the revert both use it), `was-enabled` (firewalld was enabled but not running; the revert only stops it), `enabled-unit` (on Arch, depper enabled `ufw.service` at boot; the revert disables it) |
| both | `started` (the timer or the boot service started the revert), `reverted` (who ran the revert and when), `reloaded` (sshd is about to reload the drop-in), `disarmed` (the change was verified; a revert started by the timer or at boot then does nothing), `lock` (a directory that serializes the revert and disarm) |

`revert.sh` takes an argument that says who runs it: `timer`, `boot`, `pending` (host init finishing an interrupted run) or `now` (host init rolling back). It holds the `lock` directory while it works (created with an atomic `mkdir`; it waits up to 90 seconds, then takes over a stale lock). Started by the timer or at boot after the guard was disarmed, it does nothing.

The revert acts only on the markers that exist. Each undo step removes its marker only if it succeeded; if any marker is left, the script exits 1 and does not write `reverted`, so the timer or the next host init retries. Running it twice is therefore harmless: the second run has nothing left to undo.

1. Before the change, depper writes the markers and `revert.sh` and arms a transient timer on the host: `systemd-run --collect` with `Type=oneshot`, `AccuracySec=1s`, and `RandomizedDelaySec=0`, firing 120 seconds after arming. Host init prints the exact time. The revert undoes the change by itself:
   - `--harden-ssh`: restores the previous drop-in from the `prev` copy, or removes `10-depper.conf` when there was none, then reloads sshd (always for `timer`, `boot` and `pending`; for `now`, only if the `reloaded` marker says host init had).
   - `--firewall`: when the firewall was off before, runs `ufw --force disable` or `systemctl disable --now firewalld` (only `systemctl stop firewalld` when `was-enabled` says it was enabled but not running); otherwise it removes only the ports depper added, from the recorded `zone` on firewalld. On Arch it also disables `ufw.service` again when `enabled-unit` says depper enabled it. Offline additions are removed with `firewall-offline-cmd`. A port removal counts as done when the remove succeeds or the port is no longer open.
2. depper makes the change and checks it with new SSH connections, each a fresh login without connection sharing: one as the deploy user and one as the administrator.
3. Only when both succeed does depper disarm the timer and remove the markers. Disarming stops the timer first, then takes the same lock (so a revert that has started finishes first). If the `started` marker exists, the revert started automatically and the change is reported as undone. Otherwise it writes the `disarmed` marker and removes the other markers.

If a check fails, depper rolls back at once over the administrator connection and disarms the timer. If that is impossible (for example after a lockout), host init polls the host every few seconds for the `reverted` marker, until 120 seconds plus 15 seconds have passed. It reports the time at which the timer reverted the change and that an administrator login works again; if the marker does not appear in time, it says the revert is overdue and to use the provider console. If disarming fails after a successful check, depper reports loudly that the timer will undo the change at the time it names (stop it yourself with `sudo systemctl stop depper-ssh-revert.timer`, or the `depper-firewall-revert` unit, and remove the marker files, to keep the change); if the revert had already started, it says the change was undone and to run host init again. If disarm stopped the timer but could not clear the markers, the error says so and names the marker files to remove to keep the change (`had-prev`, `prev`, `had-none`, `was-inactive`, `added-ports`, `enabled-unit`); otherwise the next host init would undo it. If `systemd-run` itself fails while arming, the error says that the revert script and markers were written and removed again (or could not be removed, and then how to proceed).

A run that is interrupted (Ctrl-C, a lost connection) is undone by the timer too. Transient timers do not survive a reboot, so the first armed guard installs and enables `depper-guard-revert.service` (`/etc/systemd/system`, running `/var/lib/depper/guard/boot.sh` after ssh, sshd, ufw and firewalld start). At boot it runs every guard's `revert.sh boot`. It stays installed and does nothing when no markers are left. If the next host init finds the timer still waiting, a revert still running, or markers present, it says "Finishing an interrupted host init": it starts the revert service if the timer is waiting (`systemctl` waits for it), waits while a revert service is still running (up to 150 seconds), stops the timer, runs `revert.sh pending` if markers remain (a failure is reported), reads the host again, and only then plans. A dry run only warns about it. A timer of an earlier run that has already elapsed is cleared with a note when the next run arms its own; a waiting one is never stopped silently.

#### `--harden-ssh`

Writes `/etc/ssh/sshd_config.d/10-depper.conf` (mode 644):

```
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
```

The `PermitRootLogin` line is written only when the underlying value is `yes`. depper determines that value without its own drop-in (`sshd -T` on a temporary copy of the configuration without `10-depper.conf`); if it cannot, it refuses. Otherwise the directive is omitted, and a `PermitRootLogin` line written by earlier depper builds over a stricter setting is removed again: depper never weakens `PermitRootLogin`, so `prohibit-password`, `forced-commands-only`, and `no` stay as they are. Root login with a key therefore stays allowed only where it was allowed before. If the administrator is root and `PermitRootLogin` is `no`, root could not log in anyway, so the administrator must be a user with passwordless sudo.

The file name sorts before the drop-ins that cloud images ship (Ubuntu's `50-cloud-init.conf`), and sshd keeps the first value it reads for each option, so depper's settings win over those. Only a line that sorts earlier, or a line in the main `sshd_config` before its `Include`, can override them.

Safety checks, all before anything is locked down:

- **Refusal at planning time.** depper refuses to continue, changing nothing, when:
  - `sshd` is not installed;
  - the main `/etc/ssh/sshd_config` has no `Include /etc/ssh/sshd_config.d/*.conf` line, because the drop-in would be ignored (add the line yourself, or drop the flag);
  - the current sshd configuration does not pass `sshd -T`;
  - `PasswordAuthentication`, `KbdInteractiveAuthentication`, `ChallengeResponseAuthentication`, or `PermitRootLogin` is set to `yes` in the main config before its `Include`, or in a drop-in that sorts before `10-depper.conf`. The error names those files, for example `sshd_config.d/01-local.conf`. Change or remove those lines first.
- **Match blocks.** A `Match` block in `sshd_config` or a drop-in that sets `PasswordAuthentication`, `KbdInteractiveAuthentication`, `ChallengeResponseAuthentication`, or `PermitRootLogin` is not refused. The plan prints a warning, and `depper host status` reports an `ssh match blocks` warning: hardening does not cover the connections such a block matches.
- **Validation after writing.** depper keeps the previous drop-in (in the guard's state directory), writes the new one, runs `sshd -t`, and checks the effective settings with `sshd -T`. If sshd rejects the configuration, or an option still has a weaker value, the previous drop-in is restored (or the file removed) and the command fails. The `PermitRootLogin` check accepts anything at least as strict as before (and never weaker than `prohibit-password`).
- **Key login proof.** Before sshd is reloaded, depper opens a new SSH connection as the deploy user with its key and runs a test script. If that fails, the previous state is restored and sshd is not reloaded.
- **Reload and re-check.** depper reloads `ssh.service` (Debian, Ubuntu) or `sshd.service` (Fedora, RHEL, Arch), whichever is active. When neither runs but `ssh.socket` or `sshd.socket` is active (sshd started per connection), there is nothing to reload: every new connection reads the new configuration. Then it opens new connections as the deploy user and as the administrator. If either fails, it rolls back as described under [Guarded changes](#guarded-changes-automatic-revert).

Undo:

```sh
sudo rm /etc/ssh/sshd_config.d/10-depper.conf
sudo systemctl reload ssh        # or sshd
```

Afterwards `depper host status` reports `ssh hardening` as a warning, because the record still says depper applied it. Edit `/etc/depper/host.json` and set `"ssh": false` under `hardening` to clear that.

#### `--firewall PORTS`

Enables a host firewall. It allows the union of these TCP ports: the ports you list, the port `host init` connects to (`--port`), every sshd `Port` and `ListenAddress` port (from `sshd -T`), and the `ListenStream` ports of `ssh.socket` when that socket is active. So the port in use cannot be closed by the list you give, and neither can a port sshd listens on, including through socket activation. For example `--firewall 80,443` allows 22, 80, and 443. If `--port` is not one of sshd's ports, depper warns and assumes a port forward or NAT in between.

- Debian and Ubuntu: installs `ufw`, runs `ufw allow N/tcp` for each port that is not allowed yet, then `ufw --force enable` if ufw is inactive. ufw's default policy applies to everything else.
- Fedora and RHEL: installs `firewalld`. depper works in the zone of the interface that the administrator's SSH connection arrives on: it takes the local address from `SSH_CONNECTION`, finds the interface that holds it with `ip -o addr`, and asks `firewall-cmd --get-zone-of-interface`. If that gives no zone it uses the default zone and warns. If firewalld is running, depper adds each missing port to both the runtime and the permanent configuration of that zone, so no reload is needed. Otherwise it adds the ports offline and enables `firewalld.service`. Port 22 counts as allowed when the `ssh` service is allowed in the zone, in both configurations.
- Arch Linux: uses ufw when it is active (adding the missing ports, and enabling `ufw.service` at boot if it is not), otherwise firewalld when it is running (as on Fedora), otherwise installs ufw and enables it as on Debian, plus `ufw.service` at boot. It refuses when a hand-written nftables or iptables ruleset is active, or firewalld is enabled but stopped; see [Arch Linux](#arch-linux).
- Existing rules are never removed. The packages are listed in `host.json`.
- The change is guarded; see [Guarded changes](#guarded-changes-automatic-revert).

depper does not check other listening services: a port that you do not list is closed to new connections once the firewall is enabled. Do not enable a firewall on a host whose other services you cannot account for.

Undo: `sudo ufw disable`, or `sudo ufw delete allow 80/tcp` for one port; on firewalld, `sudo firewall-cmd --permanent --remove-port=80/tcp && sudo firewall-cmd --reload` (add `--zone=ZONE` if it is not the default), or `sudo systemctl disable --now firewalld`. Then edit `hardening.firewall` in `host.json` to match.

#### `--auto-updates`

Enables automatic security updates.

- Debian and Ubuntu: installs `unattended-upgrades` and writes `/etc/apt/apt.conf.d/20auto-upgrades` with `APT::Periodic::Update-Package-Lists "1";` and `APT::Periodic::Unattended-Upgrade "1";`.
- Fedora and RHEL: installs `dnf-automatic`, sets `upgrade_type = security` and `apply_updates = yes` in `/etc/dnf/automatic.conf` (editing those lines in place, or creating the file), and enables `dnf-automatic-install.timer` (dnf 4) or `dnf5-automatic.timer` (dnf 5).
- Arch Linux: refused before anything changes. Unattended upgrades are not supported practice on Arch; run `sudo pacman -Syu` regularly yourself.

Automatic updates can restart services and occasionally change behaviour. Undo on Debian: set both values in `20auto-upgrades` to `"0"` or remove the file. On Fedora: `sudo systemctl disable --now dnf-automatic-install.timer` (or `dnf5-automatic.timer`) and restore the two settings in `automatic.conf` by hand; depper keeps no backup of that file.

#### Tailscale

`--tailscale-authkey-env NAME` installs Tailscale from Tailscale's official package repository (`pkgs.tailscale.com`; on Arch, the `tailscale` package from the official repositories), enables `tailscaled.service`, and runs `tailscale up` with the auth key held in the environment variable `NAME` on your machine:

```sh
export TS_AUTHKEY=tskey-auth-...        # a one-time, short-lived key from the Tailscale admin console
depper host init root@203.0.113.10 --tailscale-authkey-env TS_AUTHKEY
```

Key handling:

- You pass the name of a variable, never the key itself, so it does not appear in shell history or process lists on your machine. The command fails if the variable is unset, empty, or holds whitespace or non-ASCII characters.
- The key is sent to the host inside the script on SSH's standard input, written to a mode 600 temporary file, handed to `tailscale up --authkey=file:...` (under `timeout 120`), and deleted right after. A trap on `EXIT`, `HUP`, `INT`, and `TERM` removes the file however the script ends. It is never part of a command line on the host, printed, or recorded. In `--json` output it is shown as `<redacted>`. `host.json` records only that Tailscale was set up.
- Prefer a single-use, short-lived (or ephemeral) key, and revoke it in the admin console afterwards. A tailnet that is already up is left alone and the key is not used.
- **sudo I/O logging.** When the administrator uses `sudo`, the script that carries the key travels on sudo's standard input. If the host's sudoers enables I/O logging (`log_input`), that script, key included, is recorded in the sudo I/O log. Connect as root, or disable `log_input` for that run, and use a short-lived, single-use key either way.

depper does not change `host` in `depper.toml` to the tailnet address. To deploy over the tailnet, set `host` yourself. Tailscale gives you private reachability; it does not replace SSH authentication, and a CI runner must be able to reach the tailnet address.

Undo: `sudo tailscale down`, `sudo tailscale logout`, remove the machine in the admin console, and uninstall the `tailscale` package.

#### `--cloudflared`

Installs `cloudflared` from Cloudflare's package repository (`pkg.cloudflare.com`; an apt source with a signed keyring, or a yum repo file; on Arch, the `cloudflared` package from the official repositories, with no third-party repository). It installs the binary only: it does not log in or create a tunnel. See [Cloudflare Tunnel](tunnels.md). Undo: uninstall the package and remove `/etc/apt/sources.list.d/cloudflared.list` and `/usr/share/keyrings/cloudflare-main.gpg` (Debian family) or `/etc/yum.repos.d/cloudflared.repo` (Fedora family); on Arch, `sudo pacman -Rs cloudflared`.

### Container engine

`--container` takes `podman`, `docker`, or `none`.

Without the flag, `host init` installs the engine that the resolved `depper.toml` uses: `[container].engine`, which is `docker` when `[container]` has no `engine` key, and nothing when there is no `[container]`. `depper init` writes `engine = "podman"` for container projects, so by default a detected container project gets rootless podman. `--container none` with a `[container]` table prints a warning, since the service would then have no engine.

- **podman:** `podman` plus its rootless helpers (`uidmap` and `slirp4netns` on Debian and Ubuntu; `shadow-utils` and `passt` on Fedora; `shadow-utils` and `slirp4netns` on other RHEL-family systems), and a 65536-id range in `/etc/subuid` and `/etc/subgid` for the deploy user if it has none.
- **docker:** the distribution package (`docker.io` on Debian and Ubuntu, `moby-engine` on Fedora, `docker` on Amazon Linux 2023), the deploy user added to the `docker` group, and `docker.service` enabled. RHEL, CentOS, Rocky, and AlmaLinux have no distribution docker package, so `host init` refuses; install `docker-ce` from Docker's repository yourself, or use podman. depper prints this warning every time it adds the user to the group: membership in the `docker` group is root-equivalent on the host. Prefer rootless podman unless you need docker. See [Containers](containers.md#host-requirements).

- **podman on Amazon Linux 2023** is refused with a message: it has no supported rootless podman packages. Use `--container docker`.
- **Arch Linux:** podman with `shadow` and `passt` (or `slirp4netns`), docker from the `docker` package, and `podman-compose` or `docker-compose` for compose, all from the sync database as it is; see [Arch Linux](#arch-linux).
- **compose:** when the resolved `depper.toml` uses `[container].compose`, `host init` also installs a compose provider, so `<engine> compose` works. For podman that is `podman-compose`. For docker it is the first of `docker-compose-plugin`, `docker-compose-v2`, and `docker-compose` (only version 2 or later; the 1.x Python tool is not a docker plugin) that is installed or available from the repositories (`apt-cache policy` or `dnf info`). If none is available, `host init` refuses before changing anything and says why: with empty apt lists, run `sudo apt-get update` and retry; on RHEL-compatible systems `podman-compose` is in EPEL; docker on Debian bookworm only offers compose v1, so install `docker-ce` and `docker-compose-plugin` from Docker's repository. See [Containers](containers.md#compose).

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
  "steps": ["authorized-key", "config", "firewall", "harden-ssh", "known-hosts", "linger", "packages", "record", "subids", "user", "user-manager"],
  "packages": ["podman", "slirp4netns", "ufw", "uidmap"],
  "hardening": {
    "ssh": true,
    "firewall": [22, 80, 443],
    "auto_updates": false,
    "tailscale": false
  }
}
```

- `steps` lists the step ids depper applied, and `packages` the packages it installed (not packages that were already there). Besides the host steps it lists `known-hosts` (host key pinned), `config` (`depper.toml` written), and `record`, when they happen. The record is written after the configuration, and also after a failed doctor run or a failed apply, listing what did happen. Use it to see what to remove when you decommission the host.
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
| `ssh hardening` | Checks the effective values of `PasswordAuthentication`, `KbdInteractiveAuthentication`, and `PermitRootLogin`, not just the drop-in file. They come from `sshd -T`, run as the deploy user with a throwaway host key (`-h`) and the connection spec of the session (`-C user=...,addr=...`, so `Match` blocks apply). Pass when they are hardened. When `sshd -T` cannot be run or read as the deploy user (some images ship drop-ins that only root can read), status instead probes the authentication methods sshd offers to new, credential-less connections as the deploy user and as `root` (`Permission denied (publickey,...)`), and passes when neither offers `password` or `keyboard-interactive`. Warn when the record says depper enabled hardening and the values are not hardened, or when neither `sshd -T` nor the probe works. Info (`not enabled (opt-in)`) when it was never enabled. `firewall` also warns when ufw is enabled in `/etc/ufw/ufw.conf` but `ufw.service` is not enabled, because ufw is then off after a reboot (Arch does not enable the unit by itself). |
| `ssh match blocks` | Shown when hardening is recorded or the drop-in exists. Warn when a `Match` block in `sshd_config` or a drop-in sets password, keyboard-interactive, challenge-response, or root login: hardening does not cover the connections it matches. |
| `firewall`, `auto updates`, `tailscale` | Pass when the feature is live. Warn when the record says depper enabled it and it is not in effect now (`recorded as applied by depper but not in effect now`). Info (`not enabled (opt-in)`) when it was never enabled. `firewall` also warns when ufw is enabled in `/etc/ufw/ufw.conf` but `ufw.service` is not enabled, because ufw is then off after a reboot (Arch does not enable the unit by itself). |

A feature that `host.json` records but that is not in effect now is a warning, not a failure: something changed on the host after depper set it up.

With human output, `host status` first prints the depper version and timestamps from the record and the packages depper installed. The exit status is 1 if any check fails, otherwise 0. `--json` prints `{"host", "user", "target", "record", "checks": [{"name", "status", "detail"}, ...]}`, where `status` is `pass`, `warn`, `fail`, or `info` and `record` is the contents of `host.json` or `null`.

`host status` complements `depper doctor`, which checks what a deployment needs (rsync, `sha256sum`, disk space, locks); `host status` checks how the host was prepared.

### Troubleshooting

#### Another user's XDG variables in /etc/environment

Some images set `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, or similar in `/etc/environment` for their own default user (for example `/run/user/1001` and `/home/runner/.config`). `pam_env` applies that file to every login and systemd applies it to every user manager, so the deploy user's manager and rootless podman then try to use another user's directories and fail: units cannot start, or podman refuses a runtime directory it does not own.

depper sanitises the environment of everything it starts. Its own scripts, the process-mode start script `.depper-start.sh`, and every container engine invocation in a generated unit (`ExecStart`, `ExecStartPre`, `ExecStop`, run through `/bin/sh -c '<sanitise>; exec "$0" "$@"'`) reset `XDG_RUNTIME_DIR` to `/run/user/UID` unless the inherited one is owned by the user, and keep `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and `XDG_STATE_HOME` only when the directory is owned by the user and not writable by group or others (or when it is missing and its parent meets that test; symbolic links are followed). So the services depper starts no longer inherit another user's `XDG_*` from the user manager.

depper cannot change what the systemd user manager itself inherits: its own generators and `environment.d` still see these variables, and so does anything else that runs inside the manager. This is not just cosmetic: on the GitHub-hosted Ubuntu runner image, with its `XDG_*` lines in `/etc/environment`, the deploy user's manager does not start at all (`user@UID.service` times out), so no service runs, sanitised or not. The end-to-end test therefore only checks the services against a running manager that was given the foreign values (`systemctl --user set-environment`). On images that set them, remove the foreign lines from `/etc/environment` (as root):

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

- Fedora, RHEL, CentOS, Rocky, and AlmaLinux (`dnf`, `firewalld`, `dnf-automatic`), and Amazon Linux 2023
- Arch Linux and its derivatives (`pacman`, the sync-database check, ufw with `ufw.service`, `sshd.service`, socket-activated sshd)
- the automatic revert timers firing on a real host (only the `--harden-ssh` lockout case is covered by the end-to-end test), and `host status` reading `sshd -T` as the deploy user
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
| `compose` | `<engine> compose version` fails; only checked in compose mode, and skipped when the engine itself is missing (fail). See [Containers](containers.md#host-requirements). |
| `cloudflared` | `cloudflared` is not installed; only checked with `[tunnel]` (fail). |
| `lock` | A deployment lock is held; it reports the holder and "last refreshed X ago". A lock not refreshed for 15 minutes is stale and warns; an active one passes (see [Deployments](deployments.md#locking)). |
| `layout` | A deployment made by the Python `deployer` exists and needs `depper migrate` (`legacy`, warn); a `depper migrate` was interrupted and must be run again (`migrating`, warn); both a depper and a legacy deployment exist (`both`, warn). See [Migrating from deployer](migrating-from-deployer.md). |

The exit status is 1 if any check fails, otherwise 0. `depper doctor --json` prints `{"checks": [{"name", "status", "detail"}, ...]}`.

## Security notes

- Keep the deploy account narrow. Anyone who can use the deploy key can run arbitrary commands as that account, because `[service].command`, `setup`, and health commands are shell commands. `host init` creates the account without a password and without `sudo`.
- `host init` runs commands as root on the host you name. Verify the host key fingerprint (`--host-key-fingerprint`), use `--trust-on-first-use` only when you cannot, and run `--dry-run` first on hosts you care about.
- Do not put secrets in `depper.toml` or in the repository. Use a [dotenv file](secrets.md).
- Secrets on the host are protected by file permissions (mode 600 inside a mode 700 directory) and are readable by the service account and by root.
- Take backups of application data yourself; depper's rollback only covers the release directory, unit, and environment.
