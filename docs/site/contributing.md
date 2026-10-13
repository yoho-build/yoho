# Contributing

Integration tests deploy the fixture in `test/integration/app` to the runner's own Docker daemon over SSH. The workflow is [`.github/workflows/integration.yml`](https://github.com/yoho-build/yoho/blob/main/.github/workflows/integration.yml). It runs on every push to `main`, on pull requests, and from the Actions tab (`workflow_dispatch`).

Three jobs run in parallel on `ubuntu-24.04`:

| Job | What it checks |
|---|---|
| `compose` | `config check`, plan exit 2 then 0, deploy, zero-downtime apply, rollback, archive backup and restore, staging host route, `releases --json` |
| `swarm` | `docker swarm init`, two deploys with a request loop, rollback |
| `e2e-go` | `YOHO_E2E=1 YOHO_E2E_SSH=runner@127.0.0.1 go test -race ./...` |

## Go vulnerability scanning

The [Go vulnerability scan workflow](https://github.com/yoho-build/yoho/blob/main/.github/workflows/vulnerability.yml) runs weekly on Tuesdays at 06:23 UTC against the default branch, and manually from **Actions → Go vulnerability scan → Run workflow**. GitHub may delay scheduled runs. View the run status and findings in that workflow's Actions logs; it does not create issues or send custom notifications.

It installs the pinned `golang.org/x/vuln/cmd/govulncheck@v1.9.0` tool separately from Yoho's dependencies, uses the latest Go 1.26 patch, and queries the current Go vulnerability database at `https://vuln.go.dev`. Source-mode symbol analysis checks production `./...` packages in the repository's root module, not tests, nested fixture modules, deployed Apps, or Servers. The clean checkout contains no gitignored QA files. Findings depend on the runner's Linux/amd64 build configuration and static analysis; a clean scan is not a guarantee for other build tags/platforms or reflective calls.

Reachable vulnerabilities (including in the Go standard library), installation errors, and scan errors fail the workflow. Review the reported call paths and fixed versions before changing dependencies or the toolchain; do not suppress failures. Keep the scanner's exact module version and the checkout/setup-go commit pins reviewed when updating the workflow.

To reproduce from a clean checkout of the repository root with the latest Go 1.26 patch on `PATH`, install only into a temporary directory (no global install or `go.mod`/`go.sum` changes):

```sh
(
  set -eu
  export GOBIN="$(mktemp -d)"
  trap 'rm -rf "$GOBIN"' EXIT
  export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOTELEMETRY=off
  go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
  "$GOBIN/govulncheck" -mode=source -scan=symbol -format=text -db=https://vuln.go.dev -show=version ./...
)
```

## Run the fixture checks locally

From the repository root, with Go 1.26:

```sh
go build ./...
sh -n test/integration/run.sh
cd test/integration/app && go vet ./... && go build ./...
```

`yoho config check` needs a real Yoho file next to the compose files. The committed file is a template.

```sh
dir=$(mktemp -d)
cp -a test/integration/app/. "$dir/"
sed "s/__USER__/${USER}/g; s/__RUNTIME__/compose/" "$dir/yoho.yml.tmpl" > "$dir/yoho.yml"
go run ./cmd/yoho config check -c "$dir/yoho.yml"
```

A warning that `web` uses volume `data` without `stateful` is expected. `web` is proxied, and a stateful service cannot be proxied. Swap `__RUNTIME__` for `swarm` to check the swarm destination (the placement constraint on `web` keeps that check quiet).

## Run a deploy locally

You need Docker, a local sshd that accepts your key, and `jq`.

```sh
. ./test/integration/run.sh
setup_localhost_ssh
```

Copy the fixture as the workflow does, substitute `__USER__` and `__RUNTIME__`, `git init` and commit, build `yoho`, and put it on `PATH`. Then, in that copy:

```sh
yoho config check
yoho deploy
assert_http http://127.0.0.1:18080/up 200
```

Proxy port `18080` is published on the machine. Staging is `yoho -d staging deploy` and `curl -H 'Host: staging.local'`. Full zero-downtime, rollback, and backup steps are the `compose` job in the workflow.
