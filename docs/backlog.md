# Backlog (decided as future work)

- Roles: place subsets of Services on subsets of Servers (Kamal-style). Design must be ready before v1 config freezes.
- Manage Cloudflare Tunnel itself: create tunnels, DNS routes, ingress rules via API.
- Remote API + dashboard: App health, Server metrics, on-call alerts. The local read-only `yoho ui` / `yoho tui` exist (ADR 0010); this item is the opt-in per-Server API of ADR 0003 for unattended alerting.
- `yoho ui` actions (deploy, rollback, backup run) behind a confirmation, running the same CLI flows locally (ADR 0010 §5).
- SOPS/age secrets backend.
- Move config keys between compose `x-yoho` and the Yoho file if the split proves wrong.
- Build without rollout (`yoho build`, or a deploy flag that stops after build and ship), so a cutover can prebuild before stopping an old stack. Today the one-time `pre-deploy` hook pattern covers it.
- Warn on deploy when a proxied Service name is already an alias on the shared `yoho` network from another compose project (old tunnel routes like `http://web:3000` would resolve ambiguously).
- Server builds: hash exactly the shipped bytes into the Version, so git-ignored files shipped with `builder.gitignore: false`, `pre-build` hook outputs and uncommitted changes in submodule worktrees change it (ADR 0009 §6).
