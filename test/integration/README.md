# Integration fixture

`app/` is a tiny Go HTTP server deployed by the `integration` GitHub Actions workflow. It is not part of the `github.com/yoho-build/yoho` module.

- `GET /up` returns 200 when `/data` is writable.
- `GET /version` returns JSON `version` (`YOHO_VERSION`) and `hostname`.
- `GET /notes` lists notes. `POST /notes` appends one line to `/data/notes.txt` on the `data` volume.
- `/app migrate` (the release command) creates `/data/migrated`.

`web` is proxied on port 8080 with a catch-all host. `files` is the stateful service that owns the backup of volume `data`. `compose.staging.yaml` gives staging the host `staging.local`.

`yoho.yml.tmpl` uses `__USER__` and `__RUNTIME__` (`compose` or `swarm`). The workflow copies this directory to a fresh git repository so the deploy version is the commit SHA.

Source `../run.sh` for `assert_http`, `loop_requests`, and `fail_count`. See [Contributing](../../docs/site/contributing.md) for the local check and the Actions jobs.
