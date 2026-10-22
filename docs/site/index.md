---
layout: home
title: Yoho
titleTemplate: Deploy Docker Compose apps to your own servers

hero:
  name: Yoho
  text: Deploy Docker Compose apps to your own servers
  tagline: The way Kamal deploys one container. One command, zero downtime, no platform to run. One static Go binary, MIT licensed.
  image:
    src: /assets/logo.svg
    alt: Yoho
  actions:
    - theme: brand
      text: Get started
      link: /getting-started/
    - theme: alt
      text: 5-minute quickstart
      link: /quickstart/
    - theme: alt
      text: Set up with your AI agent
      link: /agents/
    - theme: alt
      text: GitHub
      link: https://github.com/yoho-build/yoho

features:
  - icon: 🐳
    title: Compose is the source of truth
    details: Keep compose.yaml. Yoho adds an x-yoho block per Service and a small yoho.yml; docker compose up still works locally.
    link: /configuration/
    linkText: Configuration
  - icon: ⚡
    title: Zero downtime by default
    details: Health-gated, drained cutover through kamal-proxy, also behind Cloudflare Tunnel. Rollback with the same cutover.
    link: /zero-downtime/
    linkText: Zero-downtime & proxy
  - icon: 📦
    title: No registry required
    details: Build locally (Apple container on Apple silicon, Docker elsewhere), cross-build for x86, ship layers over SSH.
    link: /builds/
    linkText: Builds & image transport
  - icon: 🔐
    title: Secrets stay out of the repo
    details: Resolved on your machine from 1Password, Bitwarden or any command, delivered as files, audited by fingerprint.
    link: /secrets/
    linkText: Secrets
  - icon: 💾
    title: Backups built in
    details: Stateful Services are pinned and never down -v. Back up to restic, S3, B2, SFTP or any rclone remote, on a schedule.
    link: /backups/
    linkText: Backups & restore
  - icon: 📊
    title: Dashboards without a platform
    details: yoho tui and yoho ui show Servers, Services, Releases and logs over SSH. Nothing runs on your Servers.
    link: /dashboards/
    linkText: Dashboards
---

<div class="yoho-home">

<div class="yoho-route" aria-label="Docker Compose apps deployed over SSH to your Server"><span>compose.yaml</span> → <span>SSH</span> → <span>your Server</span></div>

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
yoho init          # yoho.yml next to compose.yaml
yoho setup         # provision a Debian/Ubuntu Server
yoho deploy        # build, ship, switch traffic with zero downtime
yoho ui            # local dashboard of the running App
```

Yoho is one static Go binary. `compose.yaml` stays the source of truth; Yoho adds an `x-yoho` block per Service and a small `yoho.yml`. Status: alpha. License: MIT.

- [Getting started](getting-started.md)
- [5-minute quickstart](quickstart.md)
- [Set up with your AI agent](agents.md)
- [Configuration](configuration.md)
- [CLI reference](cli.md)
- Coming from another tool? [Kamal](migrating-from-kamal.md), [Coolify](migrating-from-coolify.md), [Dokploy](migrating-from-dokploy.md)

## Terms

App (one compose project), Service, Server (SSH machine), Destination (production, staging), Release (record of one deploy), Proxy (kamal-proxy), Backup, Backup Target, Scheduled Job, Stateful Service, Hook, Builder.

</div>
