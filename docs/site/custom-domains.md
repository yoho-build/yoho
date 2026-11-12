# Customer-owned domains

A SaaS App can serve many customer hostnames that are not known at deploy time. Yoho's primary path is the shared kamal-proxy already on the Server: it issues a Let's Encrypt certificate when a hostname arrives, but only after an allow check the App controls. Adding a customer domain does not require a redeploy. Unapproved hostnames get no certificate.

Cloudflare Tunnel / Cloudflare for SaaS is the alternative when TLS should terminate at Cloudflare instead of on the Server. Yoho documents that path; it does not create Cloudflare custom hostnames yet.

## What kamal-proxy v0.10.2 does

Pinned `basecamp/kamal-proxy:v0.10.2` (commit `3b4ad579bcf5db316a86d27bd896c1e30874f192`) supports on-demand TLS:

```sh
kamal-proxy deploy <service> --target host:port --tls --tls-on-demand-url=/custom-domains/allow
```

- `--tls-on-demand-url` cannot be combined with `--host`. The route is a catch-all. Other Services on the same Proxy with explicit `hosts` still take precedence (exact host, then `*.suffix`, then catch-all).
- The value is either a **path on this Service** (kamal-proxy routes the check through the App) or an **absolute `http`/`https` URL** (called directly).
- Before issuing a certificate, kamal-proxy `GET`s the endpoint with `?host=<hostname>`. A path check also sets `Host` to that hostname. HTTP **200** allows; any other status denies. Checks time out after 2 seconds and do not follow redirects.
- Hosts that already have a cached certificate skip the allow check on later handshakes, so an endpoint outage does not drop existing TLS. Removing approval denies hostnames without cached certificates; it does not revoke existing certificates or reliably stop their automatic renewal. Enforce customer access in the App as well.
- Automatic TLS does not support wildcard certificates (`*.example.com`). Each customer hostname is issued its own certificate.
- kamal-proxy has no extra ACME rate limiter. Let's Encrypt [rate limits](https://letsencrypt.org/docs/rate-limits/) still apply. The allow endpoint is the place to refuse unknown or abusive hostnames before a certificate is requested.

## kamal-proxy path (implemented)

Publish 80 and 443 (ACME HTTP-01 uses 80; TLS-ALPN uses 443). Point customer DNS (A/AAAA or CNAME) at the Server. On the Service that should receive those hostnames:

```yaml
# compose.yaml
services:
  web:
    image: shop
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}
    x-yoho:
      proxy:
        port: 3000
        tls: true
        custom_domains:
          allow: /custom-domains/allow   # or https://allow.internal/check
```

`yoho deploy` passes `--tls --tls-on-demand-url` and no `--host`. A new customer hostname that the allow endpoint accepts gets a certificate on first HTTPS request. Changing `allow` is a route update in `yoho plan`; adding a customer in the App's database is not.

`custom_domains` requires `tls: true` and cannot be set with `proxy.hosts` (kamal-proxy rejects that combination). For the App's own hostname, use a second Service with `hosts: [app.example.com]`, or approve that hostname in the allow endpoint so the catch-all serves it too.

Leave `tls` and `custom_domains` off behind a Cloudflare Tunnel: TLS belongs at Cloudflare. `yoho config check` warns when they are combined, and when both `proxy.http_port` and `proxy.https_port` are `0`.

### Allow endpoint

Implement `GET /custom-domains/allow?host=customer.example.com` on the Service (or the external URL). Return **200** only for hostnames the App has approved (database, configured list, or both). Return 403/404 otherwise. Keep the handler cheap: kamal-proxy waits at most 2 seconds and will not issue a certificate if the check fails.

Rate-limit and authenticate as you would any public-adjacent endpoint. A path check is an internal request through the Proxy to your container, but an external `allow` URL is called from the Proxy container on the `yoho` network.

A **static list in Yoho config** would still need an HTTP check at certificate time (kamal-proxy has no allowlist flag). Put known hostnames in `proxy.hosts` (that is a redeploy when the list changes) or keep the list in the App and check it here.

### Reference endpoint

[examples/custom-domains-allow](https://github.com/yoho-build/yoho/tree/main/examples/custom-domains-allow)
is a standalone Go 1.26, standard-library-only endpoint with a file-backed verified
set and a `Verified` interface for your database. It normalizes DNS names, rejects
IPs and ports, and returns 503 on source errors with a 1.5 second request budget.
Verify customer ownership first with an account-bound TXT token at
`_yoho-verify.<host>` or a customer-specific CNAME, then verify that DNS reaches
the Server before inserting the hostname.

Positive checks are cached for 5 minutes and negative checks for 30 seconds,
bounded to 10,000 entries; errors are not cached. File updates and SIGHUP reload
the source but do not flush this cache. Removal can take 5 minutes to affect new
issuance. Already-cached Proxy certificates bypass approval checks regardless.

### Staging trials

Use untrusted certificates during trials:

```yaml
x-yoho:
  proxy:
    port: 3000
    tls: true
    tls_staging: true
    custom_domains:
      allow: /custom-domains/allow
```

`tls_staging` requires `tls: true` and passes `--tls-staging` to kamal-proxy.
Changing it is a route update in `yoho plan`.
[Let's Encrypt staging](https://letsencrypt.org/docs/staging-environment/) has
higher issuance limits and certificates browsers do not trust. Use `curl -k`
only for these trials.

Set `tls_staging: false` and redeploy to use production. The next handshake
issues a production certificate unless one is already cached for that directory.
kamal-proxy separates caches by ACME directory under `certs/` in the
`yoho-proxy-config` volume, so staging certificates do not get served as
production certificates. Keep that volume across deploys.

## Limits

Production defaults, checked on 2026-11-12 against
[Let's Encrypt rate limits](https://letsencrypt.org/docs/rate-limits/):

| Limit | Budget |
|---|---|
| New orders per account | 300 / 3 hours |
| Certificates per registered domain, across accounts | 50 / 7 days |
| Certificates for an identical identifier set (duplicates) | 5 / 7 days |
| Authorization failures per identifier per account | 5 / hour |
| New account registrations per IP | 10 / 3 hours |
| New account registrations per IPv6 /48 | 500 / 3 hours |

Budgets refill gradually. ARI-coordinated renewals are exempt from all limits.
Classic renewals with the same identifiers are exempt from new-order and
registered-domain limits, but still face duplicate and failed-authorization
limits. Other request limits also apply; consult the linked policy.

kamal-proxy uses Go's
[`autocert`](https://github.com/golang/crypto/tree/v0.57.0/acme/autocert):
it renews about 30 days before expiry for 90-day certificates (or one third of
the lifetime for shorter certificates). Its order creation does not send ARI
`replaces`; renewals count as classic renewals.

When issuance fails, that hostname's TLS handshake fails. A later handshake
can retry; autocert retains failed issuance state for about a minute, so an
immediate retry may return the same failure. Repeated validation failures
consume the failed-authorization allowance. Verify DNS (including stale AAAA),
ownership, and port reachability before allowing a hostname. Existing cached
certificates for other hostnames remain usable.

### Scale test

[scripts/custom-domains-scale.sh](https://github.com/yoho-build/yoho/blob/main/scripts/custom-domains-scale.sh)
runs bounded concurrent HTTPS requests to `c<i>-<ip-with-dashes>.sslip.io`,
using staging certificates and a 60-second request timeout. The
[example scale App](https://github.com/yoho-build/yoho/tree/main/examples/custom-domains-allow)
includes the exact Compose and Yoho configuration and a warning-emitting,
test-only `ALLOW_SUFFIX=.sslip.io` mode.

```sh
scripts/custom-domains-scale.sh PUBLIC_SERVER_IP 100 5
```

Use a dedicated public Server reachable on 80/443. Configure SSH for that IP
and root or passwordless sudo access to inspect volume usage. The script reads
Proxy memory, volume size and cache file count without launching helper
containers or changing Proxy resources. Percentiles include successful 2xx
requests only and measure TLS plus HTTP latency, not ACME alone. Repeat runs
reuse certificates; record whether the cache was cold, concurrency, Server
size and DNS/network conditions when comparing results.

<!-- results: filled by a dated run -->

| Date | N | Concurrency | Success | p50 / p95 / max | Wall | Proxy memory before / after | Volume / cert files |
|---|---|---|---|---|---|---|---|
| Pending public staging Server | — | — | — | — | — | — | — |

## Scoped access

`custom_domains` is a catch-all Proxy route that also mints certificates and makes the Proxy call the allow endpoint. `yoho access secrets push` records it together with its `allow` value. A Member's deploy may use custom domains only with exactly that `allow`; a plain catch-all grant is not enough. Keep secrets out of an absolute `allow` URL, query string included: kamal-proxy gets it on the command line and `yoho plan` prints it (userinfo is refused). Protect the endpoint by network instead. See [Sharing one App with a team](team-access.md).

## Cloudflare Tunnel / Cloudflare for SaaS (documented follow-up)

Use this when you do not want Let's Encrypt or ports 80/443 on the Server.

1. Run a [Cloudflare Tunnel](cloudflare-tunnel.md) in front of `yoho-proxy` (`proxy.http_port: 0`, `https_port: 0`, `x-yoho.proxy.tls` off).
2. Give the tunnel a catch-all (or wildcard) ingress to `http://yoho-proxy:80`, so unknown customer `Host` headers reach the App.
3. Add customer hostnames with [Cloudflare for SaaS](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/) (custom hostnames): the customer CNAMEs to your fallback origin, Cloudflare issues the certificate, and the request arrives at the tunnel. Hostname ownership is validated by Cloudflare (HTTP or TXT).

Tradeoffs versus kamal-proxy on-demand TLS:

| | kamal-proxy | Cloudflare for SaaS |
|---|---|---|
| Certificate | Let's Encrypt on the Server | Cloudflare |
| New hostname | DNS to the Server + App allow check; no redeploy | Cloudflare custom hostname + customer CNAME; no Yoho redeploy |
| Unapproved host | No certificate | No Cloudflare hostname (and the App should still reject it) |
| Ports | 80/443 published | None, behind the tunnel |
| Cost / lock-in | Let's Encrypt rate limits | Cloudflare for SaaS is a paid add-on |
| Yoho today | Implemented | Documented; API create/delete is not implemented |

Yoho does not call the Cloudflare custom-hostnames API yet (creating a hostname per customer, SSL settings, validation). Do that in the dashboard, Terraform, or the App until a later Yoho command exists. A remoted-managed tunnel's ingress can already be switched at cutover; see [Migration cutover](configuration.md#migration-cutover-migrate).
