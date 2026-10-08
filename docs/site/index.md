# Yoho

Deploy Docker Compose apps to your own servers the way Kamal deploys one container: one command, zero downtime, no platform to run.

Yoho is one static Go binary. `compose.yaml` stays the source of truth; Yoho adds an `x-yoho` block per Service and a small `yoho.yml`. Status: alpha. License: MIT.

- [Getting started](getting-started.md)
- [Configuration](configuration.md)
- [CLI reference](cli.md)
- Coming from another tool? [Kamal](migrating-from-kamal.md), [Coolify](migrating-from-coolify.md), [Dokploy](migrating-from-dokploy.md)

## Terms

App (one compose project), Service, Server (SSH machine), Destination (production, staging), Release (record of one deploy), Proxy (kamal-proxy), Backup, Backup Target, Scheduled Job, Stateful Service, Hook, Builder.
