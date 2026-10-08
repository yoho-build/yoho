# kamal-proxy for zero-downtime, even behind Cloudflare Tunnel

Every zero-downtime Service sits behind one shared kamal-proxy per Server, including Services reached through Cloudflare Tunnel, where cloudflared targets the Proxy instead of the app. cloudflared does not retry failed origin requests, so plain compose recreate or scale-up-then-remove leaks 502s; a stable Proxy with health-gated cutover and draining does not. Proxy ports are configurable because Servers may already use 80/443.

## Considered Options

- Compose recreate: seconds of 502s.
- Scale up then remove old (docker-rollout): small 502 risk on removal; kept as an opt-in mode without a Proxy.
- Blue/green by rewriting tunnel ingress through the Cloudflare API: API dependency per deploy, uneven propagation.
