# AGENTS.md

Yoho: open-source Go CLI that deploys Docker Compose (or Swarm) apps to servers over SSH, replacing Kamal, ONCE, and Ansible. Read `VISION.md` for goals.

## Read first

- `GLOSSARY.md`: canonical terms (App, Service, Server, Destination, Release, Proxy, Backup, Backup Target, Scheduled Job, Stateful Service, Hook, Builder). Use them in code, docs, and output.
- `docs/adr/`: architectural decisions. Don't reverse one silently; propose a new ADR.
- `docs/decisions.md`: smaller decisions. `docs/backlog.md`: deferred work.
- `docs/research/`, `docs/consult/`: prior art and second opinions.

## Layout

- `cmd/yoho`: entry point. `internal/cli`: cobra commands. `internal/ui`: human + NDJSON output.
- `internal/config`: Yoho file (YAML/TOML/JSON/JSONC by content) and `x-yoho` structs; json tags are the schema.
- `internal/composefile`: compose loading via compose-go, `x-yoho` decoding, checks.
- `internal/secrets`: `.yoho/secrets` dotenv with `$(...)`, op/bw/bws/command providers, redaction.
- `internal/remote`: `Host` interface; SSH (system ssh + ControlMaster, scripts via `sh -s`) and Local.
- `internal/build`, `internal/transport`, `internal/version`: images, shipping, git versions.
- `internal/deploy`, `internal/proxy`, `internal/hooks`: compose runtime, kamal-proxy, Hooks.
- `internal/backup`, `internal/schedule`, `internal/setup`: Backups, Scheduled Jobs, provisioning.
- `internal/plan`, `internal/release`: CLI ↔ runtime contract, on-Server layout and Release record.

## Rules

- Go 1.26, stdlib + pinned deps. Ask before adding a dependency.
- `go build ./... && go vet ./... && go test ./...` must pass. Docker/SSH tests skip unless `YOHO_E2E=1` (`YOHO_E2E_SSH=user@host`).
- Never log secret values or put them in argv; pass via stdin, env over stdin, or files. Wrap output in the secrets redactor.
- Remote scripts: POSIX sh, `set -eu`, quote with `remote.Quote`, idempotent. Login shells may be zsh.
- Only touch Server resources Yoho owns: `<root>/apps/...`, `yoho-*` containers and projects, the `yoho` network. Never `docker compose down -v`.
- User-facing output goes through `internal/ui`: steps with timings, hints on failure, `--json` events.
- QA files live in `qa/` (gitignored). The QA Server may run unrelated production services.
- Commits end with `cosigned by OpenAI Codex at M1 Max`. Commit only when asked.
