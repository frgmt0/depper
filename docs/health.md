# Health checks and status

depper uses a health check in three places: it decides whether a deployment, rollback, or secrets push succeeded; `depper health` runs it on demand; and `depper status` reports the latest result. This page covers `[endpoint]`, `[health]`, the commands, and their JSON output.

## [endpoint]

```toml
[endpoint]
url = "http://203.0.113.10:8080"
```

`[endpoint]` records where the service is reachable without a tunnel: a LAN address, a Tailscale name, or a VPS address. It is printed after a deploy and shown by `depper status` and `depper status --json` (`endpoint_url`). depper does not request it; it is documentation that travels with the configuration. The public URL of a [Cloudflare Tunnel](tunnels.md) is reported separately as `tunnel_url`.

## [health]

```toml
[health]
url = "http://127.0.0.1:8080/health"
expect_status = 200      # default: any 2xx
timeout = 5              # seconds allowed for the HTTP exchange (URL) or the command
start_period = 30        # seconds a deploy waits for the first pass
interval = 2             # seconds between checks while waiting
```

Set exactly one of `url` or `command`. `[deploy].healthcheck = "..."` is accepted as an alias of `[health].command` (setting both is an error). Without any `[health]`, depper falls back to a unit check: a deployment waits three seconds and requires the unit to be `active` with zero restarts, and `depper health` reports whether the unit is active.

### URL checks

The URL is requested from the host's network view, not yours, so `http://127.0.0.1:8080/health` means port 8080 on the deploy host and the service does not need to be exposed. depper does this by opening `ssh -W 127.0.0.1:8080 <host>` and using its stdin/stdout as the TCP connection for an HTTP `GET`. Nothing needs to be installed on the host.

- If the SSH server refuses TCP forwarding (`AllowTcpForwarding no`), depper falls back to running `curl` on the host and reports `method` as `remote-curl`. If that is also unavailable, the check fails with an error naming both fixes: allow TCP forwarding for the account, or install curl.
- Redirects are not followed; a 3xx response is judged by `expect_status` like any other status.
- Without `expect_status`, any status from 200 to 299 passes. With it, only that exact status passes.
- HTTPS URLs are verified by depper on your machine against the host name in the URL, so a self-signed certificate or a mismatched name fails. Use an `http://` loopback URL for local health endpoints.
- A refused connection, a response that is not complete within `timeout`, or a wrong status is an unhealthy result. A failure to reach the SSH host at all, including an SSH connection that is not established in time, is an error (exit status 1), not an unhealthy result.
- `[health].timeout` covers only the HTTP exchange: writing the request and reading the response headers and body, starting once ssh is connected. Opening the SSH connection and authenticating has its own limit of `max(15 s, timeout)`, so a short `timeout` does not make a slow SSH handshake fail. The remote `curl` fallback applies `timeout` to the whole request (`--max-time`).
- The forwarded connection is a dedicated `ssh -W` process. It runs with `ControlPath=none`, so it never goes through a `ControlMaster` connection from your ssh configuration, and with `PermitLocalCommand=yes` and a `LocalCommand` that prints a ready marker on stderr once ssh has connected and authenticated. depper uses the marker to tell the SSH handshake apart from the HTTP request.
- The response body is kept up to 4 KiB.

### Command checks

```toml
[health]
command = "curl -fsS http://127.0.0.1:8080/health >/dev/null"
timeout = 10
```

The command runs on the host as `/bin/sh -lc "<command>"` in the current release directory with the `[env]` variables exported. Secrets are not exported. Exit status 0 is healthy. When the host has coreutils `timeout`, it enforces `timeout`; an exit by timeout is reported as `health command timed out after Ns`. Standard output (up to 4 KiB) is kept as the result body, and standard error becomes the error message on failure. Install any tool the command uses on the host.

If the command itself exits with status 255, the check is reported as unhealthy, not as an ssh failure. ssh uses 255 for its own connection errors, so the script on the host maps a command status of 255 to 254, and a 255 from ssh always means that the connection failed. Without standard error output, the error then reads `health command exited with status 255 (or 254)`.

### Timing

During `deploy`, `rollback`, and `secrets push`, depper restarts the unit and then polls: each round it checks that the unit is still running, runs the health check, and sleeps `interval` seconds until `start_period` has elapsed. The first passing check ends the wait. A failing check does not fail the deployment until `start_period` runs out. The deployment fails early if the unit enters the failed state or has restarted, because a crash loop is not going to become healthy. Set `start_period` longer than your service's startup time (database migrations, model loading).

`depper health` runs a single check immediately and does not wait for `start_period`.

## depper health

```sh
depper health                          # one check
depper health --json
depper health --watch 5                # repeat every 5 seconds until interrupted
depper health --count 10 --watch 2     # ten checks, two seconds apart
```

| Exit status | Meaning |
| --- | --- |
| 0 | The (last) check passed. |
| 1 | The check could not run: nothing is deployed, SSH failed, the configuration is invalid. |
| 2 | The (last) check ran and failed. |

`--count N` limits the number of checks; with `--watch` and no `--count` the loop runs until interrupted (interrupting ends the command with status 1). Without `--watch`, repeated checks run back to back. The exit status reflects the last check.

Human output is one line per check, for example:

```
http://127.0.0.1:8080/health ok (HTTP 200, 12 ms, ssh-forward)
  body: {"state":"running","progress":0.4}
```

With `--watch`, each line is prefixed with the check time.

### Health JSON

`depper health --json` prints one JSON object per check. Without `--watch` it is pretty-printed; with `--watch` each check is one compact line (JSON Lines), so you can pipe it to `jq` as it arrives.

```json
{
  "ok": true,
  "check": "url",
  "method": "ssh-forward",
  "target": "http://127.0.0.1:8080/health",
  "status": 200,
  "latency_ms": 12,
  "body": {"state": "running", "progress": 0.4},
  "error": "",
  "checked_at": "2026-10-12T15:46:10Z"
}
```

| Field | Type | Description |
| --- | --- | --- |
| `ok` | boolean | The check passed. |
| `check` | string | `url`, `command`, or `unit` (no `[health]` configured; the check is whether the unit is active). |
| `method` | string | `ssh-forward`, `remote-curl`, `remote-command`, or `systemd`. |
| `target` | string | The URL, the command, or the unit name. |
| `status` | integer | HTTP status, or 0 when the check is not an HTTP check or no response arrived. |
| `latency_ms` | integer | Duration of the check in milliseconds. |
| `body` | any | Response body (URL) or standard output (command), at most 4 KiB. Parsed JSON when the content is valid JSON, otherwise a string; `null` when empty. |
| `error` | string | Why the check failed; empty when `ok` is true. |
| `checked_at` | string | UTC time of the check, RFC 3339. |

## depper status

```sh
depper status
depper status --json
```

Human output:

```
api on deploy@203.0.113.10
  unit:      depper-api.service (active/running, pid 2314, 0 restarts)
  since:     Mon 2026-10-12 15:45:03 UTC
  release:   release-20261012T154501.237Z-3fa9c2d1 commit 9f2c1d0e4a7b (main), source worktree, by alice@laptop at 2026-10-12T15:45:01Z
  previous:  release-20261011T091200.912Z-8b1e07aa
  base:      /home/deploy/.local/share/depper/api
  endpoint:  http://203.0.113.10:8080
  health:    ok (HTTP 200, 12 ms, ssh-forward)
```

`status` exits 0 whenever it could read the host, even if the service is unhealthy; use `depper health` or `depper check` for an exit status that reflects state.

### Status JSON

```json
{
  "service": "api",
  "target": "production",
  "host": "deploy@203.0.113.10",
  "unit": "depper-api.service",
  "base": "/home/deploy/.local/share/depper/api",
  "deployed": true,
  "active_state": "active",
  "sub_state": "running",
  "main_pid": 2314,
  "n_restarts": 0,
  "active_enter_timestamp": "Mon 2026-10-12 15:45:03 UTC",
  "load_state": "loaded",
  "current_release": "release-20261012T154501.237Z-3fa9c2d1",
  "previous_release": "release-20261011T091200.912Z-8b1e07aa",
  "release": {
    "id": "release-20261012T154501.237Z-3fa9c2d1",
    "created_at": "2026-10-12T15:45:01Z",
    "source": "worktree",
    "git_commit": "9f2c1d0e4a7b5c6d8e9f0a1b2c3d4e5f60718293",
    "git_ref": "main",
    "git_dirty": false,
    "depper_version": "v0.2.0",
    "deployed_by": "alice@laptop"
  },
  "endpoint_url": "http://203.0.113.10:8080",
  "tunnel_url": "",
  "health": { "ok": true, "check": "url", "method": "ssh-forward", "target": "http://127.0.0.1:8080/health", "status": 200, "latency_ms": 12, "body": null, "error": "", "checked_at": "2026-10-12T15:46:10Z" },
  "health_error": ""
}
```

| Field | Type | Description |
| --- | --- | --- |
| `service`, `target`, `host` | string | Service name, selected target (empty for none), and configured host. |
| `unit`, `base` | string | systemd unit name and remote base directory. |
| `deployed` | boolean | A depper deployment exists on the host. |
| `load_state`, `active_state`, `sub_state` | string | systemd properties (`loaded`, `active`, `running`, ...). `active_state` is `unknown` when systemd reports nothing. |
| `main_pid`, `n_restarts` | integer | Main process id and systemd's restart counter. |
| `active_enter_timestamp` | string | When the unit last became active, as systemd prints it. |
| `current_release`, `previous_release` | string | Release ids; empty when none. |
| `release` | object or null | Metadata of the current release (`id`, `created_at`, `source`, `git_commit`, `git_ref`, `git_dirty`, `depper_version`, `deployed_by`). |
| `endpoint_url`, `tunnel_url` | string | Configured URLs; empty when unset. |
| `health` | object or null | A health result as above; `null` when nothing is deployed or the check could not run. |
| `health_error` | string | Why the health check could not run (for example SSH failure); empty otherwise. |

Both commands read the host over SSH each time they run.

## Long-running task services

A service that runs a long job (an import, a training run, a queue drain) is hard to supervise if the only signal is "the process exists". Give such a service a small HTTP endpoint on the loopback interface that reports its state, and make it the health check. depper will then show that state in every health result, and a person or a script can poll it with no extra access to the host.

Service side (any language):

- Listen on `127.0.0.1:<port>`; the health check runs on the host, so no firewall change is needed.
- Return 200 whenever the process is alive and able to answer, including while it is busy or idle. Return 5xx only when it is unhealthy. Do not return an error status merely because the job is not finished, or deployments and `depper health` will report the service as down.
- Return a small JSON body: a state, progress, and a timestamp. Keep it well under 4 KiB, since longer bodies are truncated.

```json
{"state": "running", "progress": 0.4, "processed": 4000, "total": 10000, "updated_at": "2026-10-12T15:46:08Z", "error": null}
```

Suggested `state` values: `idle`, `running`, `succeeded`, `failed`. Answer the request from a separate thread or task so a busy worker does not delay it beyond `timeout`.

Configuration:

```toml
[service]
name = "importer"
command = "python3 importer.py"
restart = "on-failure"

[health]
url = "http://127.0.0.1:9100/status"
expect_status = 200
timeout = 5
```

Polling from a script or an agent:

```sh
# Stream one JSON line every 10 seconds
depper health --json --watch 10 | jq -c '{at: .checked_at, ok, state: .body.state, progress: .body.progress}'

# Wait until the job finishes, then exit non-zero if it failed
while :; do
  out=$(depper health --json) || [ $? -eq 2 ] || exit 1      # status 2 (unhealthy) still prints JSON
  state=$(printf '%s' "$out" | jq -r '.body.state')
  case "$state" in
    succeeded) exit 0 ;;
    failed) exit 1 ;;
  esac
  sleep 10
done
```

Notes:

- Each `depper health` call opens an SSH connection, so choose an interval that suits the job; a few seconds to a minute is typical.
- `depper status --json` adds the unit state (`active_state`, `n_restarts`, `main_pid`) to the same health result. A rising `n_restarts` means the service is crash-looping even if the endpoint answers between restarts.
- For a job that is supposed to exit when done, set `restart = "no"` (or `on-failure`). When the process exits, the unit becomes `inactive`, and an endpoint health check then fails because nothing is listening; read the outcome from `depper logs` or have the job write a result file, rather than expecting the endpoint to outlive the process.
- A [command check](#command-checks) can serve the same purpose: its standard output becomes `body`. For example `command = "cat /srv/importer/status.json"` reports a status file the job maintains, and keeps working after the job exits.
