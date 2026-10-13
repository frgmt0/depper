# container

A service built into an image on the host (`[container] build`) and run by docker or podman under a systemd user unit.

On each deploy depper uploads this directory, builds `depper/container-demo:<release-id>` on the host (labelled `depper.service=container-demo`), and installs a unit that runs that image. Rolling back runs the previous release's own image; nothing is rebuilt.

`server.py` is copied into the image and runs inside the container. The container publishes its port on `127.0.0.1` only. The health check requests `http://127.0.0.1:8080/health` from the host.

## Use

1. Install docker or podman on the host and make sure the deploy account can run it ([docs/containers.md](../../docs/containers.md#host-requirements)). Note that docker group membership is root-equivalent.
2. Set `[deploy].host` in `depper.toml`.
3. Deploy:

   ```sh
   depper doctor
   depper deploy
   depper status
   ```

## Secrets

There is no env-file. Secrets and `[env]` values are passed to the container by name (`docker run --env KEY`, with the key list in the reserved variable `DEPPER_CONTAINER_ENV_ARGS`), and the values come from the unit's `EnvironmentFile`. Multi-line and quoted values work; a value may be at most 60 KiB. To try it, create an untracked `.env.production`:

```dotenv
API_TOKEN=change-me
```

uncomment the `[secrets]` table in `depper.toml`, and deploy. `/health` then reports `"api_token_configured": true`, without ever revealing the value. See [docs/secrets.md](../../docs/secrets.md).
