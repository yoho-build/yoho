# Server setup

`yoho setup` provisions a Debian or Ubuntu Server. It inspects the Server, shows each change, and asks before applying. Re-running only does what is still needed.

Steps: Docker with compose and buildx plugins, deploy user and authorized keys, Yoho directories, ufw (SSH plus Proxy ports), unattended-upgrades, optional swap and timezone, optional Docker containerd image store (for `pussh`).

Privileged steps need root or `servers.<name>.sudo: true` with passwordless sudo; without it steps are reported as blocked, not half-applied.

```yaml
setup:
  user: yoho
  authorized_keys: ["ssh-ed25519 AAAA... me@laptop"]   # default: your ~/.ssh/*.pub
  packages: [htop]
  firewall: true
  allow_ports: [2222]
  auto_updates: true
  swap: 2G
  timezone: UTC
```

The hook `docker-setup` runs during setup. Other OSes: install Docker yourself; everything else works.
