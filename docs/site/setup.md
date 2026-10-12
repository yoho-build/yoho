# Server setup

`yoho setup` provisions a Debian or Ubuntu Server. It inspects the Server, shows each change, and asks before applying. Re-running only does what is still needed.

Steps: Docker with compose and buildx plugins, deploy user and authorized keys, linger for the deploy user (systemd user timers for Scheduled Jobs and Backups keep running while logged out; skipped without systemd), Yoho directories, ufw (SSH plus Proxy ports; none behind a Tunnel), unattended-upgrades, optional swap and timezone, optional Docker containerd image store (for `pussh`).

```sh
yoho setup                          # per step: y, N, or a (all)
yoho setup --plan                   # show the checklist, change nothing
yoho setup -y                       # apply every needed step
yoho setup --containerd-image-store # also enable the containerd image store (restarts Docker)
```

Without a terminal (or with `--json`) and without `-y`, setup only shows the plan. Setup runs on every Server of the selected Destination. When you connect as a non-root user, that user is the deploy user and `setup.user` is not used.

To keep setup minimal on a shared host reached as root, set `user: root` and `authorized_keys: []`: no key is added, root is not added to the docker group (uid 0 needs no membership), and the user step reports ok. Linger and the Yoho directories only need the deploy user to exist; declining the user step skips them only when the user is missing.

Privileged steps need root or `servers.<name>.sudo: true` with passwordless sudo; without it steps are reported as blocked, not half-applied.

```yaml
setup:
  user: yoho
  authorized_keys: ["ssh-ed25519 AAAA... me@laptop"]   # unset: your ~/.ssh/*.pub; [] authorizes no keys
  packages: [htop]                                     # with builder.location: server, rsync is added automatically
  firewall: true
  allow_ports: [2222]
  auto_updates: true
  swap: 2G
  timezone: UTC
```

The hook `docker-setup` runs during setup. Other OSes: install Docker yourself; everything else works.
