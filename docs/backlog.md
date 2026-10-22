# Backlog (decided as future work)

- Fast-start health probes (`start_period`/`start_interval`) for every compose Service that is a `depends_on: condition: service_healthy` target, not only proxied and Stateful ones.
- Roles: place subsets of Services on subsets of Servers (Kamal-style). Design must be ready before v1 config freezes.
- Manage Cloudflare Tunnel itself: create tunnels, DNS routes, ingress rules via API.
- Remote API + dashboard: App health, Server metrics, on-call alerts. The local read-only `yoho ui` / `yoho tui` exist (ADR 0010); this item is the opt-in per-Server API of ADR 0003 for unattended alerting.
- `yoho ui` actions (deploy, rollback, backup run) behind a confirmation, running the same CLI flows locally (ADR 0010 §5).
- SOPS/age secrets backend.
- Move config keys between compose `x-yoho` and the Yoho file if the split proves wrong.
- Build without rollout (`yoho build`, or a deploy flag that stops after build and ship), so a cutover can prebuild before stopping an old stack. Today the one-time `pre-deploy` hook pattern covers it.
- Warn on deploy when a proxied Service name is already an alias on the shared `yoho` network from another compose project (old tunnel routes like `http://web:3000` would resolve ambiguously).
- Server builds: hash exactly the shipped bytes into the Version, so git-ignored files shipped with `builder.gitignore: false`, `pre-build` hook outputs and uncommitted changes in submodule worktrees change it (ADR 0009 §6).
- Scoped access (ADR 0011) after P1: Access Service (certificates, `yoho login`, Access Tokens, GitHub OIDC); opt-in `exec` verb; swarm runtime; App files (read-only binds from the repository) shipped through the Gate; Apple `container` images streamed to the Gate; `yoho plan` through the Gate; `yoho access limits` to edit `settings.json` limits without root on the Server; removing a scoped App's Proxy network when the App goes away. Disk limits for scoped Apps: quotas for writable layers and volumes (XFS project quotas or a per-App filesystem) and a byte cap on images the Gate pulls.
- `yoho schedule --local`: install the launchd or systemd user timer for `yoho backup pull` from the CLI. Today it is a documented recipe (docs/site/backups.md); a CLI-installed local timer departs from ADR 0003's Server-side Scheduled Job model.
- Stream remote Builder images straight to the Server (#7). `build.Direct` and its probe exist but are not wired in: a `docker-container` builder keeps the image only in BuildKit, so wiring needs a decision on `builder.direct: auto|true|false` with a `docker`-driver builder over `ssh://` (single platform, no `--load`), plus the registry `--push` variant. Needs QA on a builder/Server pair that trust each other's SSH keys.
- Docker address pools: `yoho plan` warning when the Server has no free default pool (Docker does not expose its pools; a probe needs a throwaway network); an automatic picker from a Yoho-reserved range, stable across deploys; `destinations.<name>.subnet` for the Swarm runtime (compose only today).
