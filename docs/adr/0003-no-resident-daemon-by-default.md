# No resident daemon by default

Deploys run agentless over SSH; by default Yoho is only a CLI and nothing stays running on the Server. Scheduled work (Backups, cron jobs) is opt-in per Server via `yoho schedule install`: one systemd timer per job runs the `yoho` binary and exits; a single per-Server container is the fallback without systemd. Not a sidecar per Service or per App, because App rollbacks and `compose down` would stop the job that backs the App up, and every sidecar would hold the Docker socket.

A future dashboard is a separate opt-in per-Server process bound to localhost or Tailscale, token-authenticated, with no Docker socket access.
