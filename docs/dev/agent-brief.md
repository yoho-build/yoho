# Brief for parallel coding agents (batch 1)

Yoho: open-source Go CLI, compose-native deploys over SSH, replacing Kamal + ONCE + Ansible. Module `github.com/yoho-dev/yoho`, Go 1.26.

Read first: `GLOSSARY.md`, `docs/decisions.md`, `docs/adr/*.md`. Use glossary terms in code and comments (App, Service, Server, Destination, Release, Proxy, Backup, Backup Target, Scheduled Job, Stateful Service, Hook, Builder).

Shared contracts (do NOT edit; ask the orchestrator in your final report if a change is needed):
- `internal/config/types.go` — Yoho file + `x-yoho` structs (json tags are the schema).
- `internal/remote/remote.go` — `Host` interface, `Cmd`, `Quote`, `QuoteArgs`. `internal/remote/local.go` — Local host (usable in tests).
- `internal/release/release.go` — Server layout + `Release` record.
- `internal/plan/plan.go` — `Deploy` plan + `Runtime` interface.
- `internal/secrets/generate.go` — `Generate`, `Fingerprint`.
- `go.mod` — dependencies are pinned. Do not run `go get` or `go mod tidy`. Stdlib + pinned deps only (cobra, compose-go/v2, invopop/jsonschema, go-toml/v2, hujson, go.yaml.in/yaml/v3, x/term). If you truly need another, say so in your report instead.

Rules:
- Other agents work in the same directory at the same time in other packages. Only create/edit files inside your own packages. Never run `gofmt -w` or `go fix` on the whole tree; format only your files.
- Build and test only your packages: `go build ./internal/<pkg>/... && go vet ./internal/<pkg>/... && go test ./internal/<pkg>/...`. Others' packages may be mid-edit.
- Docker may not be running locally. Unit-test by asserting generated shell scripts / commands, using `remote.Local` with temp dirs, or fake `remote.Host` implementations. Tests needing Docker/SSH must skip unless `YOHO_E2E=1`.
- Never log secret values. Never put secret values on a command line (pass via stdin or files).
- Remote shell scripts: POSIX sh, `set -eu`, quote with `remote.Quote`. Idempotent where possible.
- Comments: brief, explain why. Match Go conventions. No over-engineering.
- No git commits.
- Final report (<= 300 words): files created, exported API (signatures), what is tested, known gaps/TODOs, any contract change requests.

## Batch 2 notes

- The CLI exists: `internal/cli` (root.go, config.go, deploy.go are owned by the orchestrator; do not edit them). Add commands in NEW files and register via `func init() { extraCommands = append(extraCommands, yourCmd) }`. Reuse helpers from deploy.go/root.go: `g.load(cmd)` → `*app` (fields cfg, dest, destName, dir, ui), `a.connect(ctx)` → `[]plan.NamedHost`, `a.loadSecrets(ctx)`, `a.compose(ctx)`, `a.project()`, `performer()`, `&silentError{err}` after reporting a failure via a ui Step.
- Output must go through `internal/ui` (Step/Done/Fail(err, hint)/Skip, Info/Warn, Table, Progress, Finished). Look at internal/cli/deploy.go for the style. Every failure gets a concrete hint.
- Build the binary with `go build -o /tmp/yoho-<you> ./cmd/yoho` (not /tmp/yoho-dev).
- QA Server: `cub@192.168.3.132`, root `/home/cub/apps/yoho`, Docker 27.3.1, no passwordless sudo, ~30 unrelated production containers. Only touch `yoho-*` resources and `/home/cub/apps/yoho`. Never run `docker swarm init` there. The QA App lives in `qa/nextjs-pgvector` (deployed and serving at http://192.168.3.132:18480); deploy it with `/tmp/yoho-<you> deploy` from that dir if needed.
