# Cloudflare Tunnel

A [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) is optional. It gives a service a public HTTPS hostname through Cloudflare without opening an inbound port on the host. A service reachable over a LAN, Tailscale, or a VPS address does not need one: set [`[endpoint]`](health.md#endpoint) instead.

When `[tunnel]` is configured, depper creates one dedicated, locally managed tunnel for the service, routes exactly one hostname to it, and runs `cloudflared` as a separate user unit, `depper-tunnel@<name>.service`. The application unit does not depend on the connector, so restarting the application does not restart the tunnel.

A public route is not an access policy. Anyone on the internet can reach the hostname. Add a [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/access-controls/) application and policy separately if the service must require authentication.

## Prerequisites

1. The domain is added to Cloudflare, and its nameservers point to Cloudflare.
2. `cloudflared` is installed on the deploy host, using Cloudflare's [downloads and package instructions](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/). `depper doctor` checks for it.
3. `cloudflared` is authenticated as the same account that runs depper's services. On a headless host the command prints a URL to open in your own browser:

   ```sh
   ssh deploy@203.0.113.10
   cloudflared tunnel login
   ```

   The certificate and later the tunnel credentials stay in that account's `~/.cloudflared/`. Never commit them or copy them into the repository (`.cloudflared/` is excluded from uploads).
4. You have chosen an unused hostname. The name must not already resolve.

## Configuration

```toml
[tunnel]
hostname = "app.example.com"
service = "http://127.0.0.1:8080"
```

- `hostname` is an exact, public fully qualified name with at least three labels. Apex names and wildcards are not accepted.
- `service` is the origin as the host sees it: `http://` or `https://` with an explicit port and no path, query, or credentials.
- The tunnel's public URL is `https://<hostname>`. It is shown by `depper status` as `tunnel_url` and printed after a deploy.

`[tunnel]` belongs to the base configuration and cannot be overridden in a target. Because a hostname can be routed to only one tunnel, use `[tunnel]` in a configuration that deploys one service.

## What depper does

On each deploy, after the application is healthy and while the deployment lock is still held, depper makes sure the tunnel exists and is connected. The application deployment is already complete at that point: if the tunnel step fails, the command reports "Application deployed successfully, but tunnel setup failed" and exits with an error, and the application stays deployed and usable through its endpoint.

First run:

1. **DNS preflight.** depper resolves the hostname on your machine. If it already resolves, depper stops: the record may be an existing route or an inherited wildcard, and creating a tunnel would risk a conflict. A cached negative answer from earlier lookups of the name can make the preflight pass while the name is already claimed; a "not found" result is only as fresh as your resolver.
2. **Name check.** `cloudflared tunnel list` is read. A tunnel already called `depper-<name>` that depper did not create is never adopted.
3. **Create.** `cloudflared tunnel create depper-<name>`, with the credentials written by `cloudflared` to `~/.cloudflared/<UUID>.json`. depper identifies the new tunnel by listing again and requires exactly one match.
4. **Record.** The tunnel is recorded in `<base>/.depper/tunnel.json` (mode 600) as `name`, `id`, `hostname`, and `service`. This file defines what depper owns.

Every run (first or later):

5. Writes `<base>/.depper/cloudflared.yml` (mode 600, JSON, which is valid YAML) with the tunnel id, credentials path, one ingress rule for the hostname, and a catch-all `http_status:404`, and validates it with `cloudflared tunnel ingress validate`.
6. Creates the DNS route with `cloudflared tunnel route dns --overwrite-dns=false`. An existing record is never replaced; `TUNNEL_FORCE_PROVISIONING_DNS` is cleared from the environment for every `cloudflared` call.
7. Installs and restarts the connector unit and waits for a connection: two seconds, then up to five checks of `cloudflared tunnel list` two seconds apart. If no connection appears, it reports that the tunnel started but did not connect.

## Ownership rules

depper only modifies the tunnel recorded in `tunnel.json`. It refuses to:

- adopt a tunnel it did not create, or one whose recorded name does not match the service (`Owned tunnel state belongs to a different service`);
- overwrite an existing DNS record;
- change the hostname or origin of an owned tunnel. Edits to `[tunnel].hostname` or `service` are rejected after the first run; remove the tunnel explicitly (see below) before changing them;
- deploy a service that owns a tunnel after `[tunnel]` was removed from the configuration. Removing the block does not delete the DNS route or credentials, so depper stops and asks you to restore the block or clean up;
- replace a connector unit that lacks depper's marker line (`Description=depper managed Cloudflare Tunnel: <name>`);
- modify a `tunnel.json` that is not valid JSON, has an invalid tunnel id, or is a symlink.

## Commands

| Command | Action |
| --- | --- |
| `depper tunnel` | Create or verify the tunnel and start the connector, under the lock. Does not deploy. With `--json`: `{"name", "id", "hostname", "service", "url", "unit"}`. |
| `depper tunnel-status` | Stream `systemctl --user status` of the connector unit. |
| `depper tunnel-logs [--lines N]` | Show the connector's journal (default 100 lines). |
| `depper tunnel-stop` | Stop and disable the connector. The tunnel, its state, and its DNS route are kept; the next `depper deploy` or `depper tunnel` starts the connector again. |

## Manual cleanup

depper has no command that deletes a tunnel or a DNS record. To remove a tunnel you own, clean up exactly what you created, in this order. Use exact names and paths; do not use wildcards or bulk cleanup commands, which could remove other applications' tunnels.

Placeholders: `<name>` is the service name, `<UUID>` is the `id` in `<base>/.depper/tunnel.json`, `<hostname>` is the routed hostname, and `<base>` is `~/.local/share/depper/<name>` or your `[deploy].path`.

1. Stop the connector:

   ```sh
   depper tunnel-stop
   ```

2. In the Cloudflare dashboard, delete the DNS record for the exact hostname (`<hostname>`).
3. On the host, delete the owned tunnel by its UUID (or exact name). Deletion can fail while the tunnel still has active connections; the connector was stopped in step 1.

   ```sh
   cloudflared tunnel delete <UUID>
   ```

   The tunnel is named `depper-<name>`. A tunnel created by the Python `deployer` and migrated keeps its Cloudflare-side name `deployer-<name>`.

4. On the host, remove only the files and unit that belong to this tunnel:

   ```sh
   app_base="$HOME/.local/share/depper/<name>"        # or your exact [deploy].path
   rm -f "$app_base/.depper/tunnel.json" "$app_base/.depper/cloudflared.yml" "$app_base/.depper/tunnel-unit.next"
   rm -f "$HOME/.config/systemd/user/depper-tunnel@<name>.service" "$HOME/.cloudflared/<UUID>.json"
   systemctl --user daemon-reload
   ```

   Replace `<name>` and `<UUID>` literally; do not substitute a glob.

5. Verify that `cloudflared tunnel list` no longer shows the tunnel.
6. Remove `[tunnel]` from `depper.toml`. A deploy is refused while the owned `tunnel.json` exists, so do this after step 4.

Keep a copy of the UUID and hostname until the dashboard record and the tunnel deletion are both confirmed. Never delete another application's state or credentials during cleanup.

## Troubleshooting

- `<hostname> already resolves; refusing to risk a DNS conflict`: the name has a DNS record. Choose another hostname, or remove the old record if it is yours.
- `Tunnel "depper-<name>" already exists but is not owned by this deployment`: a same-named tunnel exists in the account without a matching `tunnel.json`. Rename or delete that tunnel yourself, or change `[service].name`.
- `cloudflared is not installed at a safe absolute path`: install `cloudflared` for the deploy account so it is found on the SSH `PATH`.
- `Tunnel service started but did not establish a Cloudflare connection`: check `depper tunnel-logs`, the host's outbound network access to Cloudflare, and that the origin in `service` is listening.
