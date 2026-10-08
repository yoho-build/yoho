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
yoho swarm init     # create the swarm on the manager (-y: no confirmation)
yoho swarm join     # join the other Servers as workers (-y: no confirmation)
yoho swarm status   # nodes and the App's services
```

`yoho swarm init` and `yoho swarm join` error when the Destination's runtime is not `swarm` (they do not connect). Set `destinations.<name>.runtime: swarm` first. `yoho swarm status` warns and still connects.

Docker advertises the manager on `servers.<name>.private_address`, else on the SSH host when that is an IP address; a hostname without `private_address` is an error. Open 2377/tcp, 7946/tcp+udp and 4789/udp between the Servers only, ideally over Tailscale or a private network. All Servers of a Destination must share the same `root`.

- Images are built locally and shipped to each node over SSH in parallel. A registry is optional. When `registry:` is set, or an image name looks like a registry host (`ghcr.io/acme/shop:1`), `docker stack deploy` passes `--with-registry-auth` so nodes pull with the credentials stored on the manager. Run `docker login` on the manager yourself; Yoho does not log in.
- The Proxy (`yoho-proxy`) runs on every node, worker nodes included, and joins the shared attachable overlay network `yoho`, which is created once on the manager. Each proxied Service is reached by its service VIP from any node. Point DNS or a Cloudflare Tunnel at any Server (`yoho tunnel up` starts a connector on each).
- Cutover uses Swarm's start-first rolling updates; the Proxy targets the service VIP. `x-yoho.strict_drain: true` routes to individual tasks instead.
- When `deploy.update_config.parallelism` is unset, Yoho starts every replica at once (still start-first) and sets `monitor: 5s` unless you set it, with the same defaults on `rollback_config`; on Docker 25 or newer a healthcheck that sets neither `start_period` nor `start_interval` is compiled with `start_period: 60s` and `start_interval: 1s` so new tasks are probed every second while they start.
- Swarm ignores compose keys such as `build`, `devices`, `privileged`, `network_mode`; hence compose is the default.
- A proxied Service needs a `healthcheck` (an error under Swarm, a warning under compose). A Service with volumes that is not `stateful: true` needs `deploy.placement.constraints`. `yoho config check` reports both. A Stateful Service always gets `node.hostname == <first Server>` added to its placement constraints; a different `node.hostname` constraint is an error. `yoho plan` also compares the live Swarm: a Service removed or scaled away out of band shows as a change.
- Stateful Services stay pinned to the first Server. Backups run there too: see [Backups](backups.md).
- Switching a Destination between compose and swarm is a migration with downtime.
