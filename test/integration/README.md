# Integration fixture

`app/` is a tiny Go HTTP server deployed by the `integration` GitHub Actions workflow. It is not part of the `github.com/yoho-build/yoho` module.

- `GET /up` returns 200 when `/data` is writable.
- `GET /version` returns JSON `version` (`YOHO_VERSION`) and `hostname`.
- `GET /notes` lists notes. `POST /notes` appends one line to `/data/notes.txt` on the `data` volume.
- `/app migrate` (the release command) creates `/data/migrated`.
- `GET /ws` answers the WebSocket opening handshake (RFC 6455) and closes.

`web` is proxied on port 8080 with a catch-all host. `files` is the stateful service that owns the backup of volume `data`. `compose.staging.yaml` gives staging the host `staging.local` and adds three Services the Proxy does not route, each with an `x-yoho.check`: `check-ws` (this app, websocket `/ws`), `check-http` (busybox httpd, http `/`) and `check-tcp` (busybox httpd, tcp). They live in the staging overlay because runtime swarm refuses `x-yoho.check`.

`yoho.yml.tmpl` uses `__USER__` and `__RUNTIME__` (`compose` or `swarm`). The workflow copies this directory to a fresh git repository so the deploy version is the commit SHA.

Source `../run.sh` for `assert_http`, `loop_requests`, and `fail_count`. See [Contributing](../../docs/site/contributing.md) for the local check and the Actions jobs.

## Recovery and faults

The workflow jobs `recovery` and `faults` run `recovery.sh` and `faults.sh` on
separate disposable Ubuntu runners. Both skip unless `YOHO_E2E=1` and refuse
mutations without `YOHO_E2E_DISPOSABLE=1`. They require Docker, passwordless sudo,
Python 3, jq, curl and SSH key authentication to `$USER@127.0.0.1` (use
`setup_localhost_ssh` from `run.sh`). They restart Docker and change the firewall;
do not use a shared QA Server. Port 18080 and App `shop` must be unused.

```sh
mkdir -p qa/resilience
go build -o "$PWD/qa/resilience/yoho" ./cmd/yoho
YOHO_E2E=1 YOHO_E2E_DISPOSABLE=1 bash test/integration/recovery.sh
# On a separate fresh machine (or after removing the recovery fixture):
YOHO_E2E=1 YOHO_E2E_DISPOSABLE=1 YOHO_E2E_FAULTS=lock,ssh \
  bash test/integration/faults.sh
```

`YOHO_BIN` overrides the absolute binary path. `RESILIENCE_LOG_DIR` receives
reports for CI artifact collection. Recovery prints the measured recovery point,
data restore duration, full duration (setup through healthy HTTP, known rows,
worker completion and active timer), image source and `needs_old_server: false`.
It uses a replacement SSH user and fresh root after deleting all original App
resources. The restic directory outside both roots represents external storage;
the OS and Docker installation are reused. Replacement paths are under root-owned
`/srv/yoho-recovery`; Server records are read as the replacement SSH identity. No fabricated timing is recorded in
docs; CI's step summary and `recovery-report` artifact contain the actual result.

Each fault can be independently selected with `YOHO_E2E_FAULTS` (`ssh`, `lock`,
`disk`, `docker`, `reboot`, `backup`). Faults assert final CLI outcome/exit status,
Server health and Release JSON integrity, then repeat apply and demand a clean
plan. Disk exhaustion occurs in a pre-deploy Hook after the operation and Release
preparation. The interrupted Backup waits for the actual paused database, sends
SIGINT, verifies unpause, compares successful inventories, then repeats Backup. Logs stay under the printed temporary fixture directory if a
scenario fails. See the failure behavior table in `docs/site/zero-downtime.md`.
