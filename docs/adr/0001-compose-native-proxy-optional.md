# Compose-native deploys, proxy optional

We are building an open-source Go CLI to replace Kamal and ONCE. A Docker Compose project is the deploy unit, not Kamal's one-container-per-role model, because real apps are multi-container and already described in compose. kamal-proxy is supported but optional, because many services are reached through Cloudflare Tunnel and need no public proxy or TLS on the host.

## Considered Options

- Kamal-style `docker run` per role: rejected, cannot express shared networks/volumes or sidecars without duplication.
- Mandatory kamal-proxy (Kamal, ONCE): rejected, redundant behind Cloudflare Tunnel.
