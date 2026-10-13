# targets

One `depper.toml` that deploys to a staging and a production host. Each `[targets.<name>]` overlays the base configuration: `host`, `name`, `known_hosts`, `keep_releases`, and `source` replace the base value, `env` tables merge key by key, and `secrets` and `endpoint` replace the base table. `[tunnel]`, `[container]`, `command`, `restart`, and `exclude` belong to the base configuration and cannot be set in a target. See [docs/configuration.md](../../docs/configuration.md#targets).

```sh
depper doctor --target staging
depper deploy --target staging
depper deploy --target production --dry-run
DEPPER_TARGET=production depper status
```

Without `--target` or `DEPPER_TARGET`, commands fail and list the target names, because the base configuration has no host.

Notes:

- Staging uses a different `name`, so its unit, directory, and releases (`depper-targets-staging.service`) stay separate even if both targets point at the same machine.
- Production sets `source = "git"`, so only committed content is deployed. `depper deploy --ref v1.2.3` deploys a tag.
- `.env.production` is a local file that is never committed or uploaded with the release. Create it before deploying to production:

  ```dotenv
  API_TOKEN=change-me
  ```

- The `known_hosts` files pin each host's key (`ssh-keyscan -p 22 203.0.113.10 > ~/.ssh/depper_production_known_hosts`; verify the fingerprint first). See [docs/hosts.md](../../docs/hosts.md#host-keys-and-known_hosts).
- `depper ci init --target production` generates a GitHub Actions workflow for the production target ([docs/ci.md](../../docs/ci.md)).
