# compose

A compose stack run as a systemd user service.

depper uploads this directory, validates `compose.yaml` and builds the images on the host, and installs a unit that runs `docker compose up --build --remove-orphans` as project `depper-compose-demo` (and `docker compose down` on stop). Ports, volumes, and commands are defined in `compose.yaml`; `depper.toml` only selects the file, and setting `ports`, `volumes`, `args`, or `command` under `[container]` is rejected.

## Secrets and environment

Every compose command depper runs, during setup and in the unit, uses `--env-file /dev/null`, so a `.env` file in this directory or in the release is ignored. `${NAME}` in `compose.yaml` is interpolated from depper's environment: the unit's `EnvironmentFile=` at runtime, and the same values exported during setup (`compose config` and `compose build`). A service receives a variable only if `compose.yaml` names it. Here `GREETING` comes from `[env]` and `API_TOKEN` from the optional dotenv file. Values are never parsed by an env-file parser, and each value may be at most 60 KiB.

To try a secret, create an untracked `.env.production` containing `API_TOKEN=change-me`, uncomment the `[secrets]` table in `depper.toml`, and deploy. `/health` reports `"api_token_configured": true` without revealing the value.

## Rollback

depper does not tag images in compose mode. `depper rollback` restores the previous release's files and runs `compose up --build` from them, so locally built services are rebuilt from that release. Pin registry images by digest if you need exact reproducibility ([docs/containers.md](../../docs/containers.md#rollback)).

## Use

```sh
depper doctor
depper deploy
depper status
```

Set `[deploy].host` first, and make sure the host has docker with the compose plugin ([docs/containers.md](../../docs/containers.md#host-requirements)).
