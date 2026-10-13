# Live tunnel check

This harmless HTTP service was used to test deployment through `desktop` and Cloudflare at `https://deployer-check.frgmt.xyz`. It returns only `deployer-cloudflare-ok`, binds locally on port 18769, and has no application data.

The included config uses the original test host and hostname. For another server, change those before deploying. To stop the existing demo:

```sh
deployer tunnel-stop examples/tunnel-check
deployer stop examples/tunnel-check
```

Tunnel stop disables its connector at boot but preserves the tunnel and DNS record for reuse. Redeploy this directory to start it again.
