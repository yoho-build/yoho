# Swarm

Set `runtime: swarm` on a Destination to deploy with `docker stack deploy`; required for more than one Server.

```yaml
servers:
  m1: { ssh: yoho@10.0.0.1 }
  w1: { ssh: yoho@10.0.0.2 }
destinations:
  production: { runtime: swarm, servers: [m1, w1] }   # first Server is the manager
```

```sh
yoho swarm init     # create the swarm on the manager
yoho swarm join     # join the other Servers
yoho swarm status
```

- Images are built locally and shipped to each node over SSH in parallel. A registry is optional. When `registry:` is set, or an image name looks like a registry host (`ghcr.io/acme/shop:1`), `docker stack deploy` passes `--with-registry-auth` so nodes pull with the credentials stored on the manager. Run `docker login` on the manager yourself; Yoho does not log in.
- The Proxy (`yoho-proxy`) runs on every node and joins the attachable overlay network `yoho`. Each proxied Service is reached by its service VIP from any node. Point DNS or a Cloudflare Tunnel at any Server.
- Cutover uses Swarm's start-first rolling updates; the Proxy targets the service VIP. `x-yoho.strict_drain: true` routes to individual tasks instead.
- Swarm ignores compose keys such as `build`, `devices`, `privileged`, `network_mode`; hence compose is the default.
- Stateful Services stay pinned to one Server.
- Switching a Destination between compose and swarm is a migration with downtime.
