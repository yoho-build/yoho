# Yoho vs Kamal 2.12 vs ONCE 0.3.4: hands-on benchmark and gap list

Date: 2026-11-03. Test machine: this container (Docker 29.8.2, sshd on 127.0.0.1:2222, no systemd as PID 1, outbound HTTPS through a TLS-intercepting proxy).
Versions: Kamal 2.12.0 (`gem install kamal`, Ruby 3.3.6), ONCE v0.3.4 (release binary `once-linux-amd64`; source at github.com/basecamp/once commit 864ec7c), Yoho `dev` built from /home/user/yoho at 3d5cba7.
Test app: a ~8 MB static Go binary. `/up` returns 503 for the first 2 s and then 200, `/` returns the build id. Kamal used it with Postgres 17 as an accessory; Yoho used `postgres` from `yoho init --template postgres`; ONCE ran Writebook.
Zero-downtime harness: one sequential curl loop (`-m 5`) running for the whole command plus 2 s after it ends; any non-200 counts as a failure.

Isolation: all my names were `kbench*`, `bench*` and `benchonce`. Kamal's proxy ran on 8080/8443, ONCE's proxy on 80/443/1318 (all free), and Yoho reused the shared `yoho-proxy` (http_port 18080, https_port 0). I never touched r3* resources or the proxy's settings. I removed only my own two routes from yoho-proxy at cleanup.

## 1. Results

| Scenario | Kamal 2.12.0 | ONCE v0.3.4 | Yoho dev |
|---|---|---|---|
| Install / init | gem + 14 dependency gems; `kamal init` 0.48 s writes deploy.yml, secrets and 9 executable sample hooks | binary download (installer script calls the GitHub API, which is blocked here; the direct release URL works) | `yoho init --template postgres` 0.02 s writes yoho.yml, secrets, compose with Postgres plus backup dump/restore and a generated password. Only `hooks/README` is written, no sample hooks |
| Pre-flight | none | none | `config check` 0.05 s: warned about restart policy `no` and missing `depends_on` for `DB_HOST=postgres` |
| First deploy | `kamal setup`: failed twice for testbed reasons (see §4), then 11.4 s warm (32 s including buildkit image pull) | `once deploy`: first attempt 22.6 s, failed on "check DNS" (the `*.localhost` default host does not resolve here, and the CLI fetches `/up` from its own machine); with `--host localhost` 7.0 s | 9.0 s (build 1.0 s, ship skipped because same daemon, cutover 6.1 s) |
| Zero-downtime redeploy | 7.9 s, 1204 req, **0 failed** | settings update (new container) 8.5 s, 896 req, **0 failed** | 7.3 s, 1123 req, **0 failed** |
| Rollback | 4.3 s, 835 req, 0 failed (reuses old container name, new container with **current** secrets) | n/a | 5.7 s, 948 req, 0 failed (short version accepted; reinstates the **old Release's secrets**, see bug B3) |
| Unhealthy deploy (`/up` always 503) | 35.8 s, 0 failed, old version kept. Error: `target failed to become healthy within configured timeout (30s)` plus one container log line | n/a (an update would keep the old container behind kamal-proxy) | 34.1 s, 0 failed, old kept. Shows the health probe output (`wget ... 503`), recent logs, per-Service convergence and route facts, and a hint |
| Logs | `-n`, `-g`, `--since`, `-f` all work; merges roles by default; `-f` exits on SIGTERM | TUI only (`g`), no CLI logs command | `-n`, `--grep`, `--since`, `-t`, `--all` work and are fast (0.32 s). Naming a Service is required when there are several (good hint). **`-f` ignores SIGTERM**, see B1 |
| Exec | `app exec` runs a new container by default, `--reuse` uses the running one; `--interactive` | `once exec host -- cmd` | `app exec web -- cmd` 0.41 s, running container only; bad Service name gives a doubled raw error with no hint |
| Secrets | `.kamal/secrets` dotenv with `$(...)`; `secrets fetch/extract/print`; 10 adapters (1password, bitwarden, bws, lastpass, aws, gcp, doppler, enpass, passbolt, test). **`docker inspect kbench-db` shows `POSTGRES_PASSWORD=benchpw`** | SECRET_KEY_BASE and VAPID keys generated and passed as env vars (per README) | `.yoho/secrets` dotenv with `$(...)`; `docker inspect` shows only `*_FILE` paths; files in `/run/secrets` (mode 0444). `secrets rotate` 6.6 s, 1119 req, 0 failed, restarted only web |
| Accessories / stateful | `accessory reboot` 3.1 s (downtime for DB), `logs`, `details` | n/a | Stateful Services are pinned, readiness-gated levels |
| Backup | **none** | `once backup` 0.26 s, container paused for 45 ms, 0 failed; auto-backup daily, 30-day retention, local path only | `backup run` 6.9 s restic, 0 failed; `backup list` |
| Restore | none | 6.6 s. Restores under a **new app name** (`writebook.fba47b`) and leaves the old volume orphaned; `teardown --remove-data` did not remove it either | `backup restore latest -y` 8.4 s; data verified (3 rows back); 4 of 724 requests got 502 while consumers were stopped (documented) |
| Drill | none | none | `backup drill` 7.7 s, phase table, RTO estimate 5.6 s, warns that no `verify` command is set |
| Auto-update | n/a | background runner every 5 min, daily per app: pull image, redeploy if the digest changed; self-update daily | n/a (CLI `yoho upgrade` with provenance check) |
| Dashboard | `kamal details` 0.76 s (proxy, app, accessory containers) | TUI: host CPU/mem/disk, per-app **Visits** (HyperLogLog over proxy logs), **req/min, error %**, CPU, mem; keys a/s/g/d/n | `yoho tui`: Server load/mem/disk, containers with health/CPU/mem, Proxy, Releases, logs. **No traffic stats.** `yoho ui` read-only browser view with one-time login link |
| Audit | `kamal audit`: Server-side `~/.kamal/kbench-audit.log` (performer, booted versions, exec'd commands, prune) | none | Releases record performer; audit exists only for the Gate (`access audit`) |
| Lock | `lock acquire -m`, `status`, `release`; deploy refused with holder and message; `--lock-wait` with timeout 900 s and poll every 15 s (new in 2.x) | none | automatic lock dir; clear refusal plus hint naming the path. No lock commands, no wait, no message |
| Hooks | pre-connect, pre-build, pre-deploy, pre/post-app-boot, post-deploy, pre/post-proxy-reboot; also run for `rollback`; `-H/--skip-hooks` | in-image `/hooks/pre-backup`, `/hooks/post-restore` | pre-connect, pre-build, pre/post-deploy, pre/post-backup, pre/post-restore fired. **pre/post-app-boot never fired; rollback ran no hooks; `YOHO_LOCK` always empty** (B2) |
| Multi-destination | `deploy.staging.yml` merged (scalars replaced); `.kamal/secrets.staging`; 6.1 s | `-n` namespace (one host) | `compose.staging.yaml` overlay: list **appended**, so staging tried to claim `bench.local` and was refused (good guard). Needed `hosts: !override [...]`; then 8.7 s, staging secret applied |
| Concurrent deploy | refused by lock | n/a | refused by lock after it had already built/shipped, which broke the running Release (B4) |
| Remove App | `kamal remove -y` 4.3 s (**also deleted images it did not own**, §5) | `once remove`, `teardown [--remove-data]` | **no command**; manual route removal, `compose down`, volume rm, 3 dirs |
| Errors / output | SSHKit INFO/DEBUG firehose; Ruby backtrace on port-forward failure; `ECONNREFUSED` raw | one-line spinners; `Error: The application couldn't be verified. Please check that you have a valid DNS record set up.` | step lines with timings, hints, NDJSON `--json`; but `secrets.values: expected config.SecretValue, got string` and doubled `Error: remote command failed on local (exit 255)` |
| Ops burden | Ruby locally, a registry, a buildx docker-container builder, `~/.kamal` and accessory data in root's home | a resident systemd/launchd service; proxy hard-wired to 80/443 (only changeable through an existing proxy container's label); DNS must resolve from the CLI | one binary; `setup --plan` lists Docker, deploy user, dirs, ufw, unattended-upgrades (not applied here) |

## 2. Yoho bugs found (evidence-based, fix first)

**B1. `yoho app logs SERVICE -f` ignores SIGTERM and leaks remote processes. P0, effort S.**
- Repro: `yoho app logs web -f -n 1 &`, `kill -TERM` it. It was still alive after 10 s. Under `timeout 4` in a pipeline it was still alive after more than 5 minutes.
- The Server keeps `sh -s` and `docker compose ... logs -f` running.
- Likely cause: `remoteRun` → `osExec` (internal/remote/ssh.go:596) kills only the ssh mux client. With ControlMaster, the mux client hands its fds to the master, so the stdout/stderr pipes stay open and `cmd.Wait` blocks. `osExec` has no `WaitDelay`; `osExecInterrupt` does.
- `--grep -f` already uses the stdin-hold design (`followGrep`).
- Fix: route plain `-f` through the same stdin-hold path, and give `osExec` a `WaitDelay`. Add a test that SIGTERMs a follow.
- This matters for agents and CI, which always wrap commands in timeouts.

**B2. Documented hooks never run. P0, effort S (doc) / M (implement).**
- docs/site/hooks.md lists `pre-app-boot`, `post-app-boot`, `pre-proxy-reboot` and `post-proxy-reboot`. They exist only in the name list (internal/hooks/hooks.go:31-34); the only call sites are pre/post-deploy (internal/deploy/compose.go:271,306; swarm.go:320,349).
- `YOHO_LOCK` is documented but never set.
- `yoho rollback` ran no hooks. Kamal ran pre-connect, pre-deploy, pre/post-app-boot and post-deploy with `KAMAL_COMMAND=rollback`. The Kamal migration guide promises "same names".
- `yoho init` docs say "sample hooks" but only a README is written.
- Fix: implement app-boot hooks around "starting new containers" and proxy hooks around `proxy boot`; run pre/post-deploy on rollback with `YOHO_COMMAND=rollback`; set `YOHO_LOCK`; write executable `*.sample` files like Kamal.
- Minor: `YOHO_HOSTS` is `127.0.0.1:2222` in deploy hooks but `root@127.0.0.1:2222` in backup hooks.

**B3. Rollback silently reinstates old secret values. P0, effort M (needs an ADR note under ADR 0006).**
- I rotated `DB_PASSWORD` twice (benchpw → benchpw2 → benchpw3), then ran `yoho rollback 543cdf718d3c`. The container read `benchpw` again. Nothing in the output mentioned it.
- A key rotated because it leaked comes back on rollback. Kamal's rollback uses the current secrets.
- Fix:
  - Default to the current secrets generation on rollback, with `--with-release-secrets` to opt into the old ones.
  - Or keep today's behaviour but print `secrets: rolling back to generation <id> (N keys differ: A, B)` and require `-y` on a terminal.
- secrets.md already notes this for *generated* secrets only.

**B4. A deploy refused by the lock has already mutated the Server. P1, effort M.**
- Two `yoho deploy` runs 2.5 s apart. The second was correctly refused at "Deploy", but only after Build and Ship.
- The Version tag `yoho/bench-production-web:<sha>` now pointed at a different image ID than the one recorded in the running Release (`release.json image_ids.web`).
- The next `yoho secrets rotate` failed: `cannot restore image ... the tag now names another build ... the original is gone`.
- Reproduced with builder == Server daemon (ship "via skip"); a remote ship also `docker load`s before the lock.
- Fix: check or take the Destination lock before build/ship (fail fast, saves the build too), or load under a temporary tag and retag inside the lock.

**B5. Small UX defects. P1, effort S each.**
- `secrets.values: expected config.SecretValue, got string` leaks a Go type. It should say what a value looks like (`{provider: vault, ref: ...}`) and suggest putting `$(...)` lines in `.yoho/secrets`.
- `yoho app exec nope -- ls` prints `service "nope" is not running` and then `Error: remote command failed on local (exit 1): ...`, with no list of Services. `app logs` already does this well.
- An unreachable Server prints the raw ssh line twice (`Error: remote command failed on local (exit 255)`) with no hint (check `ssh -p PORT user@host`, `servers.<name>.ssh`). Kamal is worse here (raw `Errno::ECONNREFUSED`), but Yoho's own standard is "hints on failure".
- Compose overlays append lists. configuration.md:64 says overlays "override"; it should say lists are appended and show `!override`. The route-conflict hint should mention `!override` when the clashing host comes from the base file.

## 3. Learn from DHH/Basecamp: changes for Yoho

| # | Design choice (source) | Yoho change | Effort | Priority |
|---|---|---|---|---|
| L1 | Kamal lock commands with a message, plus `--lock-wait` (`kamal lock acquire -m`, `status`, `release`; 2.12 adds `--lock-wait`, `--lock-wait-timeout` default 900, `--lock-wait-interval` 15) | `yoho lock acquire -m TEXT \| status \| release [--force]` on the existing `<app dir>/lock`, and `--lock-wait[=DURATION]` on deploy, rollback, backup and rotate. Replace "remove …/lock on the Server" hints with `yoho lock release`. A human-held lock blocks Scheduled Backups with a clear message | S-M | P1 |
| L2 | Kamal audit log on the Server (`~/.kamal/<service>-audit.log`; `kamal audit`) records performer, booted versions, **exec'd commands**, prune | Append-only `<root>/apps/<app>/<dest>/audit.log` (JSON lines: time, performer, command, version, secrets fingerprints, exit) for deploy, rollback, exec, backup, restore, drill, rotate, lock; `yoho audit [-n 50] [--json]`. Reuse the Gate's audit format so operator and Member entries share one schema | M | P1 |
| L3 | `kamal details` gives one screen of proxy, app and accessories | `yoho status` (alias `details`): Proxy container and routes for this App, Services with health, current Release, secrets generation, lock holder, last Backup per job. Today that takes `app ps` + `proxy status` + `releases` + `backup list` | S | P2 |
| L4 | Kamal `aliases:` (e.g. `console: app exec -i --reuse "bin/rails c"`) | `aliases:` map in yoho.yml expanding to Yoho argv; shown in `--help`; JSON Schema entry | S | P2 |
| L5 | Kamal `app exec` runs a fresh container by default (`--reuse` for the running one) and has `--interactive` | `yoho app exec -i/-t`, and `yoho app run SERVICE -- CMD` for a one-off container from the current Release image with its secrets files and networks, no route, removed after. Covers consoles and migrations without touching serving containers | M | P1 |
| L6 | Kamal `-H/--skip-hooks` | `--skip-hooks` global flag; still record it in the audit log | S | P3 |
| L7 | Kamal secrets adapters (aws, gcp, doppler, lastpass, passbolt, enpass) plus `secrets fetch/extract` batching (one vault call, many keys) | Keep op/bw/bws/command. Add documented `command` recipes for aws/gcp/doppler/passbolt in secrets.md. Batch keys per provider/item if Yoho calls `op` once per key today (fewer biometric prompts) | S | P2 |
| L8 | `kamal remove` / `once remove [--remove-data]` | `yoho remove [--volumes] [-y]` (or `apply` with an empty App): stop and remove `yoho-<app>-<dest>` containers, its routes, networks, appdir and releases. Keep volumes and backups unless `--volumes` is given and the App name is typed. Never `down -v` | M | P1 |
| L9 | ONCE backups default on: daily, 30-day retention, settings + keys in the archive so one file restores the app | `yoho init --template postgres` also writes a commented `backups:` block (local archive target + nightly job). `deploy`/`plan` warn once per Destination "Stateful Service postgres has no Backup job". Add `keep_within: 30d` retention next to `keep_last`. Opt-in scheduling stays (ADR 0003) | S | P1 |
| L10 | ONCE TUI traffic: Visits (HyperLogLog over kamal-proxy request logs), req/min, error % from kamal-proxy `--metrics-port` (127.0.0.1:1318) | `yoho proxy boot` starts yoho-proxy with `--metrics-port` bound to 127.0.0.1 (opt-in, `proxy.metrics: true`). `yoho tui`/`ui` scrape it over the existing SSH session and show req/min, 5xx % and p50/p95 per route; visits come from the proxy's JSON logs. No daemon, read-only (ADR 0003, 0010). kamal-proxy v0.10.2 has `--metrics-port` (verified) | M | P1 |
| L11 | ONCE app contract (port 80, `/up`, `/storage`, `/hooks/pre-backup`, `/hooks/post-restore`, SECRET_KEY_BASE/VAPID generated) | `yoho init --once IMAGE` (or a `once` template) writes a compose Service with `/storage` and `/rails/storage` volume, generated SECRET_KEY_BASE and VAPID, `backup.pre_backup: ["/hooks/pre-backup"]` and `post_restore` mapped, and `/up` health. Plus a "Migrating from ONCE" page (volume copy from a `once backup` tar.gz). Yoho already claims ONCE's backup contract in VISION | S-M | P2 |
| L12 | ONCE backup pauses the container for ~45 ms when no pre-backup hook exists | Show "paused for N ms" in `backup run` output and the drill table so users can compare the cost of `pause` against `pre_backup` | S | P3 |
| L13 | Kamal docs: one short page per command and per config section, generated from the same YAML the gem ships | Split long paragraphs (backups.md restore section, around 250-word paragraphs) into "What happens / On failure / Recovery" bullet lists; generate the CLI reference from cobra help so flags never drift (`yoho exec` help shows no -i today) | M | P2 |
| L14 | Kamal deploy output names each phase ("Acquiring the deploy lock…", "Prune old containers and images…") | Yoho already does this with timings. Keep it; consider a one-line "Acquired lock" step so B4-style ordering is visible | S | P3 |

## 4. What I could not run as intended

- **Kamal local registry mode** (`registry.server: localhost:5555`) failed with `Failed to establish port forward on 127.0.0.1`. Client and Server are the same host, so the SSH remote forward collides with the registry already bound on 127.0.0.1:5555. Workaround: `registry.server: 127.0.0.1:5555`, dummy credentials and `builder.driver: docker`. This is a testbed artefact, but it shows the registry is still a moving part.
- **Kamal docker-container builder** could not pull `alpine:3` from Docker Hub through the sandbox's TLS proxy (`x509: unknown authority`; the builder container lacks the CA). I switched the Kamal image to `FROM scratch`. Yoho's default docker builder used the local daemon and had no problem.
- **ONCE installer** (`curl https://get.once.com | sh`) resolves the release through api.github.com, which is blocked in this session. I used `github.com/basecamp/once/releases/latest/download/once-linux-amd64` directly.
- **ONCE background service**: no systemd as PID 1, so I skipped `once background install` and ran `ONCE_NO_SELF_UPDATE=1 once background run` for 7 minutes. Its first check ran at +5 min: "Already up to date", then an auto-backup to `/tmp/bench/once-backups/writebook.fba47b-20261103-051340.tar.gz`. Self-update was disabled on purpose.
- **ONCE default hostname** `writebook.localhost` does not resolve in this container. The CLI verifies `http://<host>/up` from its own machine and removes the app on failure. `--host localhost` worked.
- I did not run `yoho setup`, because it would change host config (user, ufw, unattended-upgrades). Only `--plan` was run.
- I did not test real 1Password or Bitwarden fetches: no `op`/`bw` here. Kamal's `secrets fetch --adapter 1password` fails clearly with `1Password CLI is not installed`; `secrets extract` works.

## 5. Where Yoho is already better (evidence)

- **Secrets off `docker inspect`.** Kamal: `POSTGRES_PASSWORD=benchpw` is visible in `docker inspect kbench-db`. Yoho: only `DB_PASSWORD_FILE`/`POSTGRES_PASSWORD_FILE` paths (ADR 0006).
- **Failure diagnostics.** On the same broken `/up`, Yoho printed the health probe output, logs and per-Service convergence facts. Kamal printed a timeout and one log line.
- **Backups, drill and restore exist.** Kamal has none. ONCE's archive is unencrypted tar.gz that includes SECRET_KEY_BASE/VAPID keys. Yoho uses encrypted restic and includes generated secrets only on encrypting targets. Yoho's drill restores into an isolated project and reports an RTO estimate (5.6 s); neither Kamal nor ONCE can prove a restore.
- **Pre-flight checks.** `config check` caught restart policy and `depends_on` mistakes in 50 ms. Kamal and ONCE check nothing.
- **Cross-Destination safety.** Yoho refused to let staging steal production's hostname (`cannot reserve hostname "bench.local" ... already reserved by different route`). Kamal relies on you setting a different host.
- **Scoped cleanup.** `kamal remove` ran `docker image prune --all --force --filter label=org.opencontainers.image.title=kamal-proxy` and `docker image rm --force postgres:17-alpine`. Per `docker events`, that deleted a pre-existing `basecamp/kamal-proxy:v0.9.0` image that was not mine and untagged `postgres:17-alpine`, which other projects use. I restored both (same digests). Yoho's rule of touching only Yoho-owned resources (AGENTS.md) and its `yoho.app` labels avoid this class of bug.
- **Lock UX.** The refusal names the holder, version, start time and path, and gives a hint (Kamal is similar). Yoho's lock also covers Backups and Scheduled Jobs.
- **No registry or Ruby.** Kamal needed 15 gems, a registry container and a buildkit builder container (plus image pulls). Yoho is one binary and the first deploy took 9 s.
- **Output.** Concise steps with timings and NDJSON `--json`. Kamal's default output is an SSHKit command firehose, with Ruby backtraces on some errors.
- **Logs.** Server-side `--grep` with `-i`/`-E`, `--until`, and bounded searches. Kamal has `-g` only.
- **Speed parity.** Redeploy 7.3 s vs 7.9 s, rollback 5.7 s vs 4.3 s, unhealthy deploy 34 s vs 36 s. Both had 0 failed requests.

## 6. Do NOT copy

- **ONCE's resident background service with daily self-update and app auto-update from `:latest`.** It conflicts with ADR 0003 (no resident daemon by default) and ADR 0008 (configuration-first plan/apply: what runs is what the config says). If anything, `yoho plan` could *report* newer digests for third-party image tags.
- **ONCE's TUI-as-configuration** (settings menu writes state into container labels). ADR 0010 makes dashboards local and read-only, and config lives in files under git (VISION principle 1).
- **ONCE's restore model** (new app name, old volume orphaned, not even removed by `teardown --remove-data`). Yoho restores in place with prior-state tracking.
- **ONCE's hard-wired 80/443 proxy and CLI-side DNS verification.** Yoho's configurable `proxy.http_port`/`https_port` and Cloudflare Tunnel support (VISION principle 3) are better for shared Servers.
- **Kamal's label-wide prune and `image rm --force` of accessory images on remove** (§5). It violates "only touch resources Yoho owns".
- **Kamal's plaintext env secrets and accessory data in `$HOME/<service>-<accessory>/`.** ADR 0006 (files, not env) and ADR 0005 (pinned Stateful Services in named volumes) are better.
- **A required registry** (VISION principle 6). Kamal's local-registry mode adds an SSH port forward and a buildkit container, and broke here when client and Server were the same host.
- **Kamal's "one container per role, no Compose".** ADR 0001/0007 keep Compose as the source of truth. Roles stay on the backlog.

## 7. Prioritised gap list

1. P0, S: B1 `app logs -f` SIGTERM hang and remote process leak.
2. P0, S/M: B2 implement or remove the documented app-boot and proxy hooks, run hooks on rollback, set `YOHO_LOCK`, write sample hooks.
3. P0, M: B3 rollback must not silently restore rotated secrets (default to current, or warn and confirm); ADR note.
4. P1, M: B4 take or check the lock before build/ship.
5. P1, S-M: L1 `yoho lock` commands and `--lock-wait`.
6. P1, M: L2 Server-side audit log and `yoho audit`.
7. P1, M: L8 `yoho remove` (volumes kept by default).
8. P1, M: L5 `app exec -it` and `app run` one-off containers.
9. P1, M: L10 proxy metrics (req/min, 5xx, latency, visits) in tui/ui.
10. P1, S: L9 Backups suggested by default for templates, warning when a Stateful Service has no job, `keep_within`.
11. P1, S: B5 error-message fixes (Go type leak, exec hint, ssh hint, `!override` docs and hint).
12. P2, S: L3 `yoho status`/`details`; L4 aliases; L7 secrets recipes and batching; L11 ONCE-image template and migration guide; L13 docs restructuring and generated CLI reference.
13. P3, S: L6 `--skip-hooks`; L12 pause duration in output; L14 lock step line.

## Sources

- Kamal hooks: https://kamal-deploy.org/docs/hooks/overview/
- Kamal proxy `run` options (http_port, https_port, metrics_port, publish): https://kamal-deploy.org/docs/configuration/proxy/ (same text as gem `lib/kamal/configuration/docs/proxy.yml`, 2.12.0)
- Kamal local registry (`server: localhost:PORT`): https://kamal-deploy.org/docs/configuration/registry/ (gem `docs/registry.yml`)
- Kamal builder driver: https://kamal-deploy.org/docs/configuration/builder/
- Kamal commands (lock, audit, secrets, prune, details, remove): https://kamal-deploy.org/docs/commands/ and `kamal <cmd> --help` from 2.12.0; gem https://rubygems.org/gems/kamal
- ONCE README and source: https://github.com/basecamp/once (commit 864ec7c; `internal/docker/proxy.go` DefaultHTTPPort 80 / HTTPSPort 443 / MetricsPort 1318; `internal/background/runner.go` CheckInterval 5 min; `internal/docker/application.go` AutomaticTaskInterval 24 h; `internal/docker/application_backup.go` BackupRetention 30 days; `internal/userstats` HyperLogLog visits)
- ONCE installer: https://get.once.com
- DHH, "ONCE again": https://world.hey.com/dhh/once-again-3e99f755
- Coverage: https://korben.info/en/once-open-source-self-hosted-platform-37signals.html, https://gittrend.io/repo/basecamp/once
- Yoho: /home/user/yoho VISION.md, docs/adr/0001, 0003, 0005, 0006, 0008, 0010, docs/site/{cli,hooks,backups,secrets,configuration,dashboards,migrating-from-kamal}.md

## Cleanup performed

- **Kamal:** ran `kamal app remove -d staging`, then `kamal remove -y` (containers kbench-*, kamal-proxy, kbench-db and its data dir). Then I removed the `kamal-docker-registry` container, the buildx builder `kamal-local-registry-docker-container`, the `kamal` network, the `kamal-proxy-config` volume, `/root/.kamal`, `/tmp/kamal-clones/kbench-*` and the `registry:3` and `moby/buildkit` images. I uninstalled kamal and its 14 newly installed dependency gems and removed the `/usr/local/bin/kamal` symlink. I re-pulled `basecamp/kamal-proxy:v0.9.0` and re-tagged `postgres:17-alpine`, both deleted by `kamal remove` (same digests as before).
- **ONCE:** ran `once -n benchonce teardown --remove-data`, removed the orphaned volume `benchonce-app-writebook.6d7a04`, removed the writebook and `kamal-proxy:once-01` images and `/root/.local/state/once`.
- **Yoho:** removed the `bench-production-web` and `bench-staging-web` routes from yoho-proxy (mine only), ran `docker compose -p yoho-bench-{production,staging} down` (no `-v`), removed the `yoho-bench-*_pgdata` volumes, the `yoho/bench-*` images and `/var/lib/yoho/{apps,appdirs,backups}/bench`, and killed the leaked `compose logs -f` processes.
- Removed `/tmp/bench`. No r3*, yoho-proxy settings or other Apps were touched.
