# Host setup

`deployer` assumes a Linux server you can reach with SSH. A small VPS or another Linux machine on your Tailscale network works well. The steps below prepare an unprivileged account named `deploy`; substitute your own account and host alias.

## 1. Prepare an Ubuntu VPS (or use an existing Linux host)

On a new Ubuntu server, create a dedicated account and install the base tools. Run these commands as an administrator:

```sh
sudo adduser --disabled-password --gecos '' deploy
sudo apt update
sudo apt install -y openssh-server python3 python3-venv rsync
```

Python 3.11 or newer is required by the deployer itself; check the installed version and install a newer distribution package or a separately managed runtime when necessary:

```sh
python3 --version
```

For a private server, install Tailscale on the VPS using the [official Linux instructions](https://tailscale.com/docs/install/linux) and run `sudo tailscale up` there. Install the Tailscale app on a Mac using the [official macOS instructions](https://tailscale.com/docs/install/mac), then sign in from the app. Confirm both devices appear in the same tailnet before using the VPS's Tailscale address or name as `HostName` below.

## 2. Create an SSH alias

On your workstation, add an entry to `~/.ssh/config`:

```sshconfig
Host desktop
    HostName 100.64.0.12
    User deploy
    IdentityFile ~/.ssh/id_ed25519_deploy
    IdentitiesOnly yes
```

If you do not already have a deployment key, create one locally:

```sh
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_deploy
```

As an administrator on the VPS (for example, through the provider console), append the public key without replacing existing keys:

```sh
ssh root@SERVER 'install -d -m 700 -o deploy -g deploy /home/deploy/.ssh'
cat ~/.ssh/id_ed25519_deploy.pub | ssh root@SERVER 'umask 077; cat >> /home/deploy/.ssh/authorized_keys; chown deploy:deploy /home/deploy/.ssh/authorized_keys; chmod 600 /home/deploy/.ssh/authorized_keys'
```

Replace `SERVER` with the VPS's initial address. On systems that provide it, `ssh-copy-id -i ~/.ssh/id_ed25519_deploy.pub deploy@SERVER` is an equivalent append operation. Then verify the alias:

```sh
ssh desktop 'id -un && uname -s'
```

Use a Tailscale address or DNS name as `HostName` when the host should remain private. Keep the deploy account narrowly scoped: it needs access to its application directory and user-level systemd, not a general administrator role.

## 3. Install target dependencies

On the Linux host, install Python 3.11 or newer and rsync using the distribution's package manager. The service command and each command in `[deploy].setup` may need additional runtime packages, a virtual environment, or system libraries; install those explicitly as part of setup. The workstation also needs SSH and rsync.

Confirm the target tools:

```sh
python3 --version
rsync --version
systemctl --user --version
```

## 4. Enable user services after logout and reboot

User services normally depend on a user login session. For a long-running server, enable lingering for the deployment account so its user manager starts at boot and remains available after logout:

```sh
sudo loginctl enable-linger deploy
```

This is the one setup step that commonly needs administrator assistance. Check the result with:

```sh
loginctl show-user deploy -p Linger
```

The deployer manages a systemd user unit, so use `systemctl --user` while diagnosing it. Do not grant the service root privileges merely to make a user unit work.

## 5. Deploy and inspect

From the repository:

```sh
deployer deploy --dry-run
deployer deploy
deployer status
deployer logs
```

The default remote directory is `~/.local/share/deployer/<service name>`, resolved by the remote account. Set `[deploy].path` when the application needs a different absolute directory and the account has permission to write there.

If the application needs dependencies, put repeatable installation commands in `[deploy].setup`. Keep those commands safe to run more than once. `setup` runs on the remote host and should be written for that host's shell and distribution.

If the application exposes a local readiness endpoint, set `[deploy].healthcheck` to a command that exits successfully only when the service is ready, for example `curl -fsS http://127.0.0.1:3001/api/config >/dev/null`. Install any command used by the check on the target host.

## Security and operations

Use a dedicated account and SSH key with the smallest useful filesystem permissions. Avoid putting passwords, tokens, or private keys in `deploy.toml`, Git, or shell history. For deployment-time secrets, use `[secrets] file = ".env.prod"` or `deployer deploy --env /path/to/.env.prod`. The CLI path is resolved from the directory where you invoke deployer; the TOML path is resolved from the repository. `--env` overrides the configured file for that deployment. Absolute paths and `~` are also supported. Values from the selected dotenv file override duplicate `[env]` keys.

The selected file is transferred over the SSH connection to the protected remote `.deployer/environment` file (directories mode 700, file mode 600), and is never rsynced into the release or used as a working-directory `.env` file. Both the configured file and a CLI override inside the repository are excluded by their anchored relative paths; when it is a symlink, the resolved in-repository target is excluded too. Secret values are available to the running service only; setup and healthcheck commands receive `[env]` values but not deployment-time secrets. The dotenv format is one `KEY=VALUE` assignment per line, with blank lines and `#` comments allowed, an optional `export ` prefix, and optional matching single or double quotes around values. Double-quoted values may contain escaped or literal newlines; Both single- and double-quoted values may span lines; unquoted values remain single-line. Double-quoted escapes include `\n`, `\r`, `\t`, `\\`, and `\"`; there is no interpolation or shell sourcing. Files must be UTF-8 (BOM accepted) and no larger than 1 MiB. A missing selected file fails the deployment. Previous secret values remain in protected rollback transaction backups, so account for that retention when rotating credentials.

`--env FILE` applies only to `deploy`; operational commands do not need the secret source. Removing `[secrets]` or omitting `--env` on a later deployment replaces the remote environment with `[env]` alone, removing keys that were supplied only by the previous secret file.

The protected environment file is a permission boundary, not an encrypted vault: the service account can read it, and root can read it. Application code can also expose values through its own logs or diagnostics.

`logs` reads the service's journal. `rollback` can restore a retained prior application release when available, but it does not undo migrations, database writes, uploaded files, or any other external side effects. Take application-level backups for those resources.

The supported target is Linux with systemd user services. Tailscale provides private network reachability; it does not replace SSH authentication or host hardening.

## Optional Cloudflare Tunnel

Use a tunnel when an application needs a Cloudflare-hosted HTTPS hostname. This is optional; a Tailscale-only service needs no Cloudflare account or tunnel. The domain must be added to Cloudflare and its nameservers must point to Cloudflare before a hostname can be routed.

Install `cloudflared` on the Linux target using the [official downloads and package instructions](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/). The locally-managed tunnel workflow and configuration fields are documented in Cloudflare's [create a local tunnel](https://developers.cloudflare.com/tunnel/advanced/local-management/create-local-tunnel/) and [configuration file](https://developers.cloudflare.com/tunnel/advanced/local-management/configuration-file/) guides.

Authenticate as the same Linux account that runs deployer and the application:

```sh
ssh desktop
cloudflared tunnel login
```

On a headless server, the command prints a browser URL. Open that URL on your workstation, sign in, and select the Cloudflare zone for the hostname. Keep the resulting account certificate and tunnel credentials in that user's `cloudflared` directory; never commit them or put them in `deploy.toml`. The deployer uses a dedicated tunnel named for the service and a separate user service for `cloudflared`, so restarting the application does not depend on the tunnel process sharing its unit.

Add this to `deploy.toml` only after authentication is complete:

```toml
[tunnel]
hostname = "app.frgmt.xyz"
service = "http://127.0.0.1:3001"
```

Choose a hostname that is not already in use. The deployer refuses an existing tunnel it does not own and does not force DNS overwrites. If the name or DNS record is already claimed, resolve that in the Cloudflare dashboard or choose another subdomain. A route makes the service reachable over public HTTPS; it does not authenticate users. Add a Cloudflare Access application and policy separately when the route must be private, using the [Cloudflare Access documentation](https://developers.cloudflare.com/cloudflare-one/access-controls/).

After deployment, inspect or control the connector independently:

```sh
deployer tunnel-status
deployer tunnel-logs
deployer tunnel-stop
```

`tunnel-stop` disables the `deployer-tunnel@<name>.service` connector but keeps the tunnel state and DNS route. A later `deployer deploy` starts the connector again. Keep the hostname and origin unchanged after the first deployment; v1 refuses drift rather than silently changing a live route. If tunnel setup fails after the application was deployed, the application remains deployed and usable through its local or Tailscale address; inspect both service statuses before retrying.

### Manual tunnel cleanup

Removing `[tunnel]` from `deploy.toml` is refused while owned tunnel state exists. Clean up only the exact tunnel you own, in this order:

1. Stop the connector: `deployer tunnel-stop`.
2. In the Cloudflare dashboard, delete the DNS record for your exact hostname (for example, `app.frgmt.xyz`).
3. On the target host, delete the exact owned tunnel by its recorded UUID or name: `cloudflared tunnel delete <OWNED-UUID-OR-NAME>`. Do not use a wildcard or a broad tunnel cleanup command.
4. On the target host, after the connector is stopped, remove only the exact owned files and unit: the deployment's `.deployer/tunnel.json`, `.deployer/cloudflared.yml`, and `.deployer/tunnel-unit.next` files, `$HOME/.config/systemd/user/deployer-tunnel@<name>.service`, and `$HOME/.cloudflared/<OWNED-UUID>.json`.
5. Reload the user manager with `systemctl --user daemon-reload`, then verify `cloudflared tunnel list` no longer shows the owned tunnel.

For the default application directory, the file removal is equivalent to this exact-path command after replacing the placeholders:

```sh
app_base="$HOME/.local/share/deployer/<name>"
rm -f "$app_base/.deployer/tunnel.json" "$app_base/.deployer/cloudflared.yml" "$app_base/.deployer/tunnel-unit.next"
rm -f "$HOME/.config/systemd/user/deployer-tunnel@<name>.service" "$HOME/.cloudflared/<OWNED-UUID>.json"
systemctl --user daemon-reload
```

If `[deploy].path` is set, use that exact configured path for `app_base`. Do not replace `<name>` or `<OWNED-UUID>` with a glob.

Keep backups of the UUID and hostname until the dashboard record and tunnel deletion are confirmed. Never delete another application's state or credentials while cleaning up.

## Interrupted deployments

Deploy and rollback take a per-service lock so overlapping operations fail instead of changing the same release pointers. If the deploying computer dies or loses its connection, a lock may remain at `<deployment path>/.deployer/lock`. After confirming that no deployer operation or remote setup command is still running, remove that empty directory with `rmdir`, then retry. Do not remove a lock while another deployment is active.
