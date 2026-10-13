# basic

A single process service: `python3 server.py` under a systemd user unit.

`server.py` runs on the deploy host, not on your machine. It is a standard-library-only Python script, so the host needs `python3` and nothing else. depper itself needs no Python on either side; replace the script with any program that your `command` can start.

The server listens on `127.0.0.1:8080`, answers `/` with a greeting, and answers `/health` with a small JSON status document. The health check requests that URL from the host, so the port is never exposed.

## Use

1. Set `[deploy].host` in `depper.toml` to your host (the example uses a documentation address, `203.0.113.10`).
2. Prepare the host as described in [docs/hosts.md](../../docs/hosts.md).
3. From this directory:

   ```sh
   depper doctor
   depper deploy --dry-run
   depper deploy
   depper status
   depper health --json
   ```

To reach the server from your machine, tunnel through SSH:

```sh
ssh -L 8080:127.0.0.1:8080 deploy@203.0.113.10
curl http://127.0.0.1:8080/
```
