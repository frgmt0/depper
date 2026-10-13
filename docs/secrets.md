# Secrets

depper separates configuration that can be committed (`[env]` in `depper.toml`) from secrets, which stay in a local dotenv file that is never committed, never uploaded with the release, and never printed.

## Model

- The local dotenv file is the source of truth. The host holds a copy that the service reads; depper never reads values back from the host.
- At deploy time the dotenv values are merged over `[env]` and written to a single private file on the host, `<base>/.depper/environment`.
- depper keeps a manifest of key names and salted fingerprints next to it. This lets it tell you which keys differ between your local file and the host without ever transmitting or storing a comparable hash of a value.

## Dotenv format

depper uses its own small parser. It does not run a shell, does not expand `$VAR` or `${VAR}`, and does not execute command substitutions; all of those stay literal text.

```dotenv
# comments start with # at the beginning of a line
DATABASE_URL=postgres://app:s3cret@db.internal:5432/app
export API_TOKEN="abc123"                  # optional "export " prefix
GREETING='hello $USER'                     # single quotes: literal, no escapes
MESSAGE="line one\nline two"               # double quotes: \n \r \t \\ \" are interpreted
PRIVATE_KEY="-----BEGIN KEY-----
MIIE...
-----END KEY-----"                         # quoted values may span lines
PLAIN=value with spaces # inline comment   # unquoted: single line, " #" starts a comment
```

Rules:

- One `KEY=VALUE` assignment per line. Keys match `[A-Za-z_][A-Za-z0-9_]*`. Whitespace around `=` and leading whitespace are ignored.
- Unquoted values end at the end of the line; trailing whitespace is trimmed, and whitespace followed by `#` starts a comment.
- Double-quoted values interpret `\n`, `\r`, `\t`, `\\`, and `\"`. Other backslash sequences are kept as written. They may contain literal newlines.
- Single-quoted values are literal and may span lines.
- After a closing quote only whitespace or a comment may follow.
- If a key appears more than once, the last assignment wins.
- The file must be UTF-8 (a BOM is accepted), at most 1 MiB, and contain no NUL bytes.
- Errors report a line number and never echo file content.

Dotenv values override `[env]` values with the same key.

## Choosing the source

| Source | Use |
| --- | --- |
| `[secrets] file = ".env.production"` | Default for the target. Relative to the repository root. |
| `--env FILE` | Replaces the configured file for one command. Relative to the current directory. |
| `--env-stdin` | Reads dotenv content from standard input. Cannot be combined with `--env`. |

`--env` and `--env-stdin` are accepted by `depper deploy`, `depper check`, `depper secrets diff`, and `depper secrets push`.

```sh
depper deploy --env ./secrets/prod.env
printf '%s' "$DEPPER_ENV" | depper deploy --env-stdin        # CI
```

Selected files are validated before anything is sent: a missing, unreadable, non-regular, oversized, or malformed file stops the command with no change to the host. Symlinks are followed to the real file. A configured or selected file that lies inside the repository is excluded from the rsync upload by its anchored path (and so is its resolved target), in addition to the default `.env` and `.env.*` excludes.

Removing `[secrets]`, or deploying without a source, writes an environment containing only `[env]`; keys that existed only in the old dotenv file disappear from the host.

## Where values live on the host

| Path (under `<base>/.depper/`) | Mode | Content |
| --- | --- | --- |
| `environment` | 600 | `KEY="value"` lines in systemd `EnvironmentFile=` syntax. Backslash and double quote are escaped; newlines are kept literally. In container build and image modes it also holds the reserved line `DEPPER_CONTAINER_ENV_ARGS`, the list of `--env KEY` arguments (key names only). |
| `environment.manifest` | 600 | JSON: `file_sha256`, `keys` (name to fingerprint), `updated_at`. No values. |
| `fingerprint.salt` | 600 | 32 random bytes, hex encoded. Created on first use. |
| `transactions/<id>/` | 700 | Backups of previous environments (see [Retention](#retention-and-pruning)). |

The `.depper` and `transactions` directories are mode 700. Files are uploaded through the SSH connection on standard input, written to a private temporary file next to the destination, and renamed into place, so a reader never sees a partial file. Values never appear in command lines (so not in `ps`), never go through rsync, and never appear in depper output.

Container modes supply the same values to the container through the unit's environment instead of an env-file (`--env KEY` pass-through in build and image mode, `${VAR}` interpolation in compose mode). Values up to 60 KiB are accepted, and multi-line and quoted values are allowed. See [Containers](containers.md#how-secrets-reach-containers).

### What each stage can see

| Stage | `[env]` | Dotenv secrets |
| --- | --- | --- |
| The running service | yes | yes |
| `[deploy].setup` commands | yes | no |
| Command health checks | yes | no |
| Compose `config` and `build` during setup (compose mode only) | yes | yes |
| URL health checks | not applicable | not applicable |
| Release directory contents | no | no |

Setup and health commands run on the host from the release directory and see `[env]` only. If a build step needs a secret, that is outside what depper provides, with one exception: in compose mode depper exports the full environment (`[env]` and secrets) to `compose config` and `compose build`, so that `${VAR}` in the compose file resolves the same way as at runtime. The values are exported inside the script sent on standard input, never placed in a command line or written to a file.

## Manifest and fingerprints

Every write of the environment file also writes the manifest:

```json
{"file_sha256":"<sha256 of the environment file>","keys":{"API_TOKEN":"<hex>","DATABASE_URL":"<hex>"},"updated_at":"2026-10-12T15:45:01Z"}
```

- Each fingerprint is `HMAC-SHA256(salt, value)` in hex, using the raw salt bytes as the key. Because the salt is random and per deployment, fingerprints cannot be compared across hosts and are not an unsalted hash of a guessable value.
- `file_sha256` is the SHA-256 of the exact environment file depper wrote. It detects edits made on the host outside depper.
- Fingerprints are compared locally: `secrets diff` fetches the salt, computes the HMAC of your local values, and compares them with the manifest. Fingerprints are never printed.

HMAC fingerprints of low-entropy values (for example a four-digit PIN) can still be guessed offline by someone who holds both the manifest and the salt. Both files are mode 600 in the same directory as the environment file itself, which already contains the values, so this does not widen access.

## Inspecting: list and diff

```sh
depper secrets list
depper secrets list --json
```

Prints the variable names recorded in the manifest and when they were last written. A warning is shown when the live file no longer matches `file_sha256`, meaning it was edited outside depper. JSON: `{"service", "target", "known", "keys", "updated_at", "file_modified"}`. `known` is false until a deploy or push has written a manifest (for example on a deployment migrated from the Python tool).

```sh
depper secrets diff
depper secrets diff --env ./secrets/prod.env --json
```

Compares your local merged environment (`[env]` plus dotenv) with the host.

```
api: secrets drift (1 added, 1 changed, 3 unchanged); local environment differs from the deployed one
  added    NEW_FEATURE_KEY
  changed  API_TOKEN
```

States: `in-sync`, `drift` (keys differ, or the host file was modified outside depper), and `unknown` (no manifest on the host, or nothing deployed). The exit status is 0 for `in-sync` and `unknown`, 3 for `drift`. JSON: `{"service", "target", "state", "reason", "keys": {"known", "added", "removed", "changed", "unchanged"}, "file_modified", "updated_at"}`, with key names only.

`depper check` includes the same comparison under `secrets`.

## Updating: push

```sh
depper secrets push
depper secrets push --env ./secrets/prod.env
```

`push` replaces only the environment, without creating a release. Under the deployment lock it:

1. backs up the live environment, manifest, and unit into a transaction directory;
2. uploads the new environment and manifest as candidates and moves them into place;
3. restarts the unit and waits for health, as a deploy would;
4. on failure, restores the backed-up files and restarts again, then reports the failure;
5. on success, records the new environment as the applied configuration of the current release and removes the transaction.

If the automatic restore in step 4 fails, the error lists the recovery steps, and a bare `depper rollback` restores the environment, manifest, and unit saved before the push ([Deployments](deployments.md#interrupted-operations)). For a container deployment made by an earlier depper, run `depper deploy` once before the first `secrets push` ([Containers](containers.md#how-secrets-reach-containers)).

Both `deploy` and `push` print a names-only summary of the change, for example `1 added (NEW_FEATURE_KEY), 1 changed (API_TOKEN), 3 unchanged`. When there is no earlier manifest to compare with, all keys are listed as new.

## Rotating a secret

1. Create the new credential at its provider while the old one still works, if the provider allows two at once.
2. Edit the local dotenv file.
3. `depper secrets diff` and confirm that only the intended key names are listed as changed.
4. `depper secrets push`, and confirm `depper health` passes.
5. Revoke the old credential at the provider.
6. Decide about old copies on the host: see below.

## Retention and pruning

Previous environments are kept so that rollback and automatic restore can work:

- `transactions/<release-id>/environment` is the environment that was live before that release was deployed.
- `transactions/<release-id>/applied/environment` is the environment that release runs with, used by `rollback --to`.

So after a rotation, the old value still exists in the backups of the most recent deployments until their releases are pruned. A rollback to an older release also restores that release's old environment, including old credentials. To limit retention:

- Revoke the old credential at its provider; this is the reliable fix.
- Lower `keep_releases` (minimum 2) and deploy: pruning a release deletes its transaction backups together with the release.
- As a last resort, delete a specific `transactions/<release-id>` directory by hand. That release then shows `[no saved configuration]` in `depper releases` and can no longer be a `rollback --to` target.

## Threat model

depper's protection is a permission boundary, not a vault.

What it protects against:

- Secrets leaking through the repository, the rsync upload, command lines, process listings, release directories, or depper's own output and logs.
- Other unprivileged accounts on the host: files are mode 600 in a mode 700 directory.
- Partial or torn writes, and symlink tricks on the destination (depper refuses symlinked environment files and directories).
- Silent drift between what you think is deployed and what is deployed.

What it does not protect against:

- Root on the host, and any process running as the service account. The service account must be able to read its own environment, and `/proc/<pid>/environ` of the service is readable by the same user. Child processes started by the service inherit the variables.
- The application itself logging or exposing its variables.
- Container tooling: with docker or podman, `inspect` on the container shows its environment to users who can talk to the engine (see [Containers](containers.md#how-secrets-reach-containers)).
- Compromise of the deploy key or your workstation. Anyone who can deploy can run commands as the service account and therefore read the secrets.
- Disk-level exposure: values are not encrypted at rest.

If you need encryption at rest, per-secret access control, audit trails, or dynamic credentials, use a dedicated secrets manager and give the service a short-lived token through the environment.
