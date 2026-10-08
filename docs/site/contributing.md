# Contributing

Integration tests deploy the fixture in `test/integration/app` to the runner's own Docker daemon over SSH. The workflow is [`.github/workflows/integration.yml`](https://github.com/yoho-build/yoho/blob/main/.github/workflows/integration.yml). It runs on every push to `main`, on pull requests, and from the Actions tab (`workflow_dispatch`).

Three jobs run in parallel on `ubuntu-24.04`:

| Job | What it checks |
|---|---|
| `compose` | `config check`, plan exit 2 then 0, deploy, zero-downtime apply, rollback, archive backup and restore, staging host route, `releases --json` |
| `swarm` | `docker swarm init`, two deploys with a request loop, rollback |
| `e2e-go` | `YOHO_E2E=1 YOHO_E2E_SSH=runner@127.0.0.1 go test -race ./...` |

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
