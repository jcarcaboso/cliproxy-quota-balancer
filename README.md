# CLIProxyAPI quota balancer

A scheduler plugin for multiple Codex or Claude accounts. It selects the account
whose usable five-hour **or weekly** window resets soonest, while preserving a
configurable reserve in each window.

## Policy

For each account, read the active five-hour and weekly quota observations:

1. Exclude an account if an observed, unexpired quota is exhausted.
2. Exclude it if **any** window has only the reserve remaining and resets later
   than `release-within`.
3. Rank the remaining accounts by their earliest absolute reset timestamp,
   regardless of whether that reset belongs to a five-hour or weekly limit.
4. Break equal reset times by account ID.
5. If no measured account is eligible, round-robin among accounts with unknown
   quota. Never include a known reserved or exhausted account in that fallback.
6. Reject selection when all candidates are reserved or exhausted.

A weekly reset in 15 minutes beats another account's five-hour reset in 30
minutes. It does **not** override a reserve held by the first account's other
window. Both windows still limit how much that account can serve.

```yaml
enabled: true
priority: 100
strategy: earliest-reset
reserve-percent: 10
release-within: 1h
```

- `reserve-percent` is the percentage of each window's quota to preserve.
  Default `10`; accepted range `0 <= value < 100`. Zero disables reservation.
- `release-within` is the time before a window resets at which its reserve is
  released. Default `1h`. Values such as `30m` and `2h` work; `0s` keeps the
  reserve until reset. At exactly the threshold, the reserve is released.
- `strategy` also accepts `round-robin` or `fill-first`. Those choices delegate
  to the host and bypass this reserve policy.
- Invalid configuration leaves the last working configuration unchanged.

The host applies credential eligibility, cooldowns, model support, pinned
accounts and priority tiers first. Use equal credential priorities if all
accounts should compete. Give this plugin a higher plugin priority than other
scheduler plugins. Remote CLIProxyAPIHome account selection is outside its scope.

## Observations and limitations

No polling or quota API calls are made. Codex observations use the primary and
secondary windows with `Window-Minutes` of `300` or `10080`. Either can be the
five-hour window. Claude observations use unified `5h` and `7d` headers.
Relative resets are anchored to the observation timestamp, not the selection
time. Expired or malformed observations are unknown, not proof of fresh quota.

The plugin cannot predict the cost of the next request. Concurrent requests or
a large request may cross the reserve boundary before an updated observation
arrives. Accounts must receive traffic before passive quota is known. There is
no promise of using all remaining quota if there is insufficient traffic, and no
carry-over across provider resets.

The host SDK is pinned to
`jcarcaboso/CLIProxyAPI@43b0f3d936401f0c449313886108e6a554dbd5be`.
That commit exposes bounded quota signals to scheduler plugins without
credentials or cooldown internals. The combined image builds that exact host.
An unpatched host will have no quota observations and use unknown-account fallback.

## Build and test

Requires Go 1.26.5+ and a C compiler:

```sh
make test
make verify
```

Tests include threshold boundaries, invalid values, weekly/five-hour comparisons,
other-window reservation, exhausted quota, expired snapshots, unknown-data
fallback, configuration backup/idempotence, and 5,000 seeded random account pools
checked against an independent policy oracle.

`make verify` loads the compiled `.so` through the real CLIProxyAPI host. It
checks host quota-header collection, C ABI transport, streaming and non-streaming
picks, and hot reconfiguration. These use synthetic quota data, not live
credentials or billable provider requests.

CI runs the same tests and builds and verifies the combined container.
The container also runs real `/v1/responses` HTTP requests, both streaming and
non-streaming, against a local fake Codex upstream. It seeds two accounts with
different observed quotas and checks which credential actually reaches the
upstream. This covers the full proxy-to-plugin path, weekly versus five-hour
ordering, reserve release, exhaustion, and rejection without upstream traffic.

## Install the plugin

Copy `dist/quota-balancer.so` into the patched host's plugin directory and merge
into its existing configuration:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    quota-balancer:
      enabled: true
      priority: 100
      strategy: earliest-reset
      reserve-percent: 10
      release-within: 1h
```

The initial release supports Linux x86_64 with glibc. Build on the target OS
for other platforms. The Debian combined image and its plugin share the same
Go/toolchain/libc build environment.

## Combined container

The image includes the pinned host, plugin, configuration installer and native
verification tool. No runtime download or compilation is needed:

```sh
docker build -t skorcius/cliproxy-quota-balancer:dev .
docker run --rm --entrypoint /usr/local/bin/verify-quota-balancer \
  skorcius/cliproxy-quota-balancer:dev -plugin-dir /CLIProxyAPI/plugins
```

Mount the existing host configuration at `/config/config.yaml`, auth data at
`/root/.cli-proxy-api`, and an optional non-secret policy file at
`/etc/quota-balancer.yaml`. The entrypoint atomically merges only this plugin's
settings and host plugin discovery settings into the persisted configuration.
It leaves other settings and plugins intact, keeps a first-run backup at
`/config/config.yaml.before-quota-balancer`, and uses mode `0600`.
The policy is reapplied on each restart, so edit the mounted policy rather than
changing this plugin's managed settings through the dashboard.

Roll back using the old host image and restore the saved config while the proxy
is stopped. Keep the auth/config/log/static volumes. Do not delete OAuth state.

Tags `v*` trigger a release. After native/container verification, the workflow
publishes a versioned Docker Hub image and a GitHub plugin archive with SHA256
checksums. Publishing requires `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`
repository secrets. Production deployments should pin the resulting image digest.

## License

MIT. The C ABI wrapper is derived from CLIProxyAPI's scheduler example;
the upstream notice is retained in `LICENSE`.
