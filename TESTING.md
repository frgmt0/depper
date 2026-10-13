# Verification

Run the local suite:

```sh
python3 -m unittest discover -v
```

Live checks performed on `desktop` on September 6, 2026:

- Deployed a temporary Python HTTP service, then a second release with changed environment data.
- Rolled back and verified the original HTTP response and environment were restored.
- Killed the process and verified systemd started a new process successfully.
- Deployed a deliberately broken command and verified automatic restoration of the working release.
- Verified status and stop; removed the temporary service registration afterward.
- Built and deployed Card Cabinet over SSH. Its health endpoint and UI loaded through Tailscale; systemd reported active, enabled, and zero automatic restarts.

Card Cabinet is available at http://100.108.167.104:3001 on the tailnet. Its config is in the Card Cabinet repository. Server-side card data is stored separately at `/home/jason/.local/share/card-cabinet/data`; the local collection was not copied.

The deployed Card Cabinet snapshot passed 12 of 14 app tests. The two failures were a cross-year recognition assertion and its native macOS Vision OCR test. The OCR implementation calls `/usr/bin/swiftc` and Apple's Vision framework, so that recognition path is not portable to the Linux target. The UI and API deployment should not be mistaken for complete Linux support for all recognition features. Card Cabinet is being edited independently, so later deployments may contain newer app behavior.

## Cloudflare Tunnel

The opt-in tunnel demo in `examples/tunnel-check` is running at `https://deployer-check.frgmt.xyz`. Verified:

- Created one new dedicated tunnel and exact DNS hostname; the four existing named tunnels were unchanged.
- HTTPS returned `deployer-cloudflare-ok` through Cloudflare.
- Redeployment reused the same tunnel UUID and DNS route successfully.
- A new-deployment DNS preflight rejected that now-resolving hostname before creating anything.
- The tunnel reported established edge connections and its own enabled systemd user service.

The Mac resolver initially cached the earlier nonexistent hostname; authoritative DNS and Cloudflare's resolver showed the new record, and a normal HTTPS request from `desktop` succeeded. The demo contains no card data. Its README includes commands to stop the application and connector.

## Dotenv deployment secrets — September 17, 2026

The automated suite covers dotenv syntax and literal values, CLI/config precedence, secret-file exclusion using real local rsync, dry-run redaction, deploy-only loading, and private atomic uploads.

A temporary service on `desktop` was also tested with synthetic credentials only:

- Runtime values matched expected hashes for quotes, backslashes, multiline values, and literal shell substitutions. No substitutions executed.
- The TOML source was applied automatically. A CLI override replaced matching values and removed keys absent from the new file.
- Both the overridden TOML source and the selected source remained outside release folders.
- The remote environment file was mode 600; its metadata and transaction directories were mode 700.
- Setup received only `[env]` values, never dotenv values.
- A failed start restored the previous runtime secrets; explicit rollback restored the earlier version even with the local source missing.
- Status worked without the local source; deployment with a missing selected source failed before changing the live service.
- No secret candidate files remained after success/failure, and CLI output did not include the synthetic secret values.

The temporary service and its remote files were removed after verification. Existing applications and their real credentials were not changed.
