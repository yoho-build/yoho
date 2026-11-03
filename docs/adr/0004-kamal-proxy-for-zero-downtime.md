# kamal-proxy for zero-downtime, even behind Cloudflare Tunnel

Every zero-downtime Service sits behind one shared kamal-proxy per Server, including Services reached through Cloudflare Tunnel, where cloudflared targets the Proxy instead of the app. cloudflared does not retry failed origin requests, so plain compose recreate or scale-up-then-remove leaks 502s; a stable Proxy with health-gated cutover and draining does not. Proxy ports are configurable because Servers may already use 80/443.

## Upgrading kamal-proxy

Added later, no change to the decision: the Proxy image is pinned and part of the Proxy configuration fingerprint. `yoho plan`/`deploy` report and apply `upgrade vX -> vY`, and `yoho proxy upgrade` recreates `yoho-proxy` explicitly (pull first, routes kept in the state volume, routes verified afterwards). kamal-proxy cannot hand over between versions, so the upgrade interrupts traffic for about a second; see `docs/site/zero-downtime.md`.

## Considered Options

- Compose recreate: seconds of 502s.
- Scale up then remove old (docker-rollout): small 502 risk on removal; kept as an opt-in mode without a Proxy.
- Blue/green by rewriting tunnel ingress through the Cloudflare API: API dependency per deploy, uneven propagation.
