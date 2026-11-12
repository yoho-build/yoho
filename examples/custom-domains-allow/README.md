# Reference customer-domain allow endpoint

This standalone Go 1.26 module uses only the standard library. Run
`go test ./...`, then `go run .`. It listens on `:8080` by default
(`LISTEN_ADDR` overrides it). GET
`/custom-domains/allow?host=customer.example.com` returns 200 only for a verified
hostname, 403 for an unknown hostname, 400 for invalid input, 405 for other
methods, and 503 if the source fails or exceeds the 1.5 second budget.

Before inserting a hostname, require the customer to publish a unique,
account-bound TXT token at `_yoho-verify.<host>`, or verify a CNAME to your
customer-specific target. Check ownership and that A/AAAA/CNAME resolves to
the intended Server before marking it verified. Recheck ownership when domains
change accounts. This endpoint does not perform DNS verification itself.
Never approve arbitrary input: that would let strangers consume your ACME
orders and failed-validation allowance.

Set `VERIFIED_FILE` (default `verified-hosts.txt`) to a file containing one
verified hostname per line; blank lines and # comments are accepted. The included
sample lists only `example.com` hostnames so `go run .` works immediately. Replace it
atomically. Hostnames are lowercased with one trailing dot stripped; IPs,
ports, non-ASCII names (use punycode), invalid labels and oversized names are
rejected. The source reloads on mtime/size changes or SIGHUP, and source read or
parse errors deny new checks. Production database implementations can replace
the `Verified` interface; the source includes a parameterized Postgres query
without importing a driver.

Positive answers are cached for 5 minutes, negative answers for 30 seconds,
with at most 10,000 entries and an expired-entry sweep at capacity. Source
errors are not cached. File changes/SIGHUP do not invalidate cached answers:
allow up to 5 minutes for removal or 30 seconds for addition to take effect.
Already-issued certificates in kamal-proxy bypass subsequent allow checks.
Keep this endpoint reachable only from your Proxy where practical; apply
request limits at your network edge. Sources must honor context cancellation.

## Staging scale App

`ALLOW_SUFFIX=.sslip.io` is an explicit **test-only** bypass of ownership
verification. It logs a startup warning. Never use it with production ACME.
The included `compose.scale.yaml` deploys this endpoint as the scale App itself,
with an HTTP healthcheck, on-demand TLS and untrusted staging certificates.

Copy this module to an isolated App directory, use
`compose.scale.yaml` as `compose.yaml`, and create `yoho.yml`:

```yaml
app: domain-scale
servers:
  scale:
    ssh: deploy@YOUR_PUBLIC_IPV4
destinations:
  staging:
    servers: [scale]
```

Run `yoho deploy -d staging` from that directory on a dedicated public Server
with ports 80/443 available. Then from the Yoho checkout:

```sh
scripts/custom-domains-scale.sh YOUR_PUBLIC_IPV4 100 5
```

Configure SSH so the IP logs in as your deploy user. The script only inspects
the existing Proxy and volume; root or passwordless sudo for du/find is
required for volume statistics. It records first-request TLS plus HTTP time,
2xx success count, successful-request percentiles, wall time, Proxy memory,
volume size and number of certificate-cache files (which may include account
metadata). Repeat runs reuse certificates; use a fresh dedicated Server for
comparable cold issuance runs. sslip.io and ACME are external dependencies;
results depend on DNS, network, Server size and concurrency. Staging limits
still apply.

Use a verified-host file instead of ALLOW_SUFFIX outside this isolated test.
