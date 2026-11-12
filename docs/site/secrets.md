# Secrets

By default, secrets are resolved on your machine at deploy time, written to the Server as versioned files (0444 in 0700 directories owned by the deploy user), and mounted via compose `secrets:` at `/run/secrets/<NAME>`. File delivery keeps secret values out of the repo, the image, and `docker inspect`. Explicit `secrets_as_env: true` uses `env_file` instead: values become container environment variables visible through `docker inspect`.

## Where values come from

1. `.yoho/secrets`: dotenv with `$(...)` substitution, Kamal style. `.yoho/secrets.<destination>` is layered on top.

   ```sh
   SECRET_KEY_BASE=$(op read op://Production/shop/secret_key_base)
   POSTGRES_PASSWORD=$(bw get password shop-db)
   ```

2. `secrets.providers` + `secrets.values` in `yoho.yml`:

   ```yaml
   secrets:
     providers:
       vault: { type: op }
     values:
       BACKUP_PASSWORD: { provider: vault, ref: "op://Production/shop-backups/password" }
   ```

   Provider types: `op` (1Password), `bw` (Bitwarden), `bws` (Bitwarden Secrets Manager), `command` (argv with the reference appended).

3. `x-yoho.generate`: created once on the Server, never in your password manager unless you back them up. Replace one with `yoho secrets rotate --generated NAME` ([below](#rotating-generated-secrets)).

## 1Password approvals

Yoho reads all `op` references of an account in one `op inject`, so each Yoho command asks for 1Password approval at most once. How long that approval lasts is up to 1Password:

- **Desktop app integration.** In a terminal, approval lasts for that terminal session: 10 minutes of inactivity, at most 12 hours, and locking the app revokes it. Coding agents (Claude Code, Codex) and scripts run every command in a new process without a terminal. On macOS, 1Password CLI 2.40 and later shares one approval across those processes, so a parent session and its sub-agents are asked once instead of once per command. Check with `op --version` and update the CLI if it is older. Older versions, and other platforms, may ask again for every Yoho command.
- **Service account, no prompts.** For unattended runs (CI, a long agent session), give a 1Password service account read access to only the vaults the App needs and export its token before running Yoho:

  ```sh
  export OP_SERVICE_ACCOUNT_TOKEN="$(security find-generic-password -w -s yoho-op-sa)"  # keep the token in the keychain, not in .yoho/secrets
  yoho deploy
  ```

  `op read` and `op inject` work with service accounts. Service accounts cannot read Personal or Private vaults, and their hourly and daily request limits are shared across the account, so prefer item and vault IDs in refs (`op read` makes one request instead of three). Server-side jobs never call 1Password (ADR 0006), so a crash loop on the Server cannot use up those limits.

Yoho does not cache resolved values between commands and does not keep a session token on disk. Do not start an agent under `op run` to avoid prompts: that puts every secret into the agent's environment.

## Requiring nonempty values (opt-in)

Empty secret values remain valid by default. To require particular **source keys**
(the keys in `.yoho/secrets` or `secrets.values`, not container aliases), declare:

```yaml
secrets:
  nonempty: [DATABASE_PASSWORD, BACKUP_PASSWORD]
```

Every listed key must exist and resolve locally to something other than exactly
`""` for the selected Destination. Whitespace-only and multiline values are valid;
this check does not trim or change values. Existing provider/`$(...)` newline
handling is unchanged. Keys must match `[A-Za-z_][A-Za-z0-9_]*`; duplicates are
errors. A missing key, including a typo, an alias without its own source value,
or a provider value excluded from this Destination, is an error, not ignored.
Destination overlays can replace the list when contracts differ.

`deploy`, `apply`, and `plan` check resolved values before connecting to any
Server or running the pre-connect Hook. Any other command that resolves local
secrets checks the entire list too, including Backup Target credentials, managed
`tunnel up`, `secrets list`/`print`/`rotate`, and credential-bearing `schedule install`.
`config check` also resolves local secrets **only when this list is nonempty**;
without it, config inspection still runs no secret commands. This opt-in uses the
full existing resolution pipeline, not just listed keys: all applicable providers
and secret-file `$(...)` commands run, so an unrelated resolution failure can fail
`config check` even when every declared key has a literal value. Secret files are
trusted shell code; commands can depend on any previously defined key or provider
value (including indirect environment lookups), so Yoho does not infer a partial
shell dependency graph. Do not opt in expecting a credential-free structural check;
`config show` still does not resolve credentials. `yoho config check --offline` (or `YOHO_OFFLINE=1`) is the only mode that never runs a provider or `$(...)` line, even under `secrets.nonempty`; use it in CI.

Provider references overridden by secret files are still not fetched. With an
active contract, **all** failed provider/secret-file commands report keys only:
this includes undeclared keys sharing a provider call and credentials not yet known
to the Store. Provider references, command diagnostics, raw/encoded output, and
underlying command-error text are suppressed. Cancellation/deadline status is
retained without command output. With an empty/absent list, existing diagnostic
behavior is unchanged.

Server-generated source keys (`x-yoho.generate`) cannot appear in this list,
even if a local value with that key also exists. Yoho rejects that combination
when inspecting Compose, before provider calls or SSH: these keys are created
or retained on the Server and cannot satisfy a local resolution contract.
Commands resolving an opt-in contract therefore also need valid local Compose
files. `schedule install` also checks this local Compose/generation contract before
SSH for credentialless jobs, without resolving their unrelated local providers or
secret-file commands. Generated-secret delivery and auditing are unchanged.

Commands that do not resolve local secrets do not gain a credential requirement
(e.g. `config show`, App status/logs, Scheduled Job status/run, and rollback).
Stored Scheduled Job specs and retained Releases continue using their stored
credentials; current local declarations are not retroactively enforced on them.
Reinstall/apply checks newly resolved credentials, but this declaration does not
rotate secrets, alter empty values, or repair credentials automatically.

## Rotating values

Change the value in 1Password, Bitwarden, or `.yoho/secrets`, then:

```sh
yoho secrets rotate              # every Service whose values changed
yoho secrets rotate web          # fail unless only web changed
yoho secrets rotate --dry-run    # list what would restart; change nothing
```

Yoho re-resolves secrets on your machine, writes a new generation, and restarts only the Services whose values changed: proxied Services through the Proxy (zero downtime), Stateful Services stop-first. Images are not rebuilt. Run it from a checkout of the deployed Version (same commit, same uncommitted changes): rotate recompiles the local compose files and refuses when they are another Version. Any other difference from the deployed project (a git-ignored `.env`, say) is refused too. `pre-deploy` and `post-deploy` Hooks run with `YOHO_COMMAND=secrets rotate`. The current Release is recorded again with the new secrets generation, so its old values are not kept: to undo, restore the old value in the provider and rotate again. Rolling back to an earlier Version keeps the rotated values ([below](#rollback-keeps-the-current-secrets)). Output names Services, never values. With scoped access, the operator also runs `yoho access secrets push` so Members pick up the new generation.

## Rotating generated secrets

`yoho secrets rotate` re-reads your provider, so it cannot change a value that only exists on the Server. Changing `POSTGRES_PASSWORD` alone would not change the database role either: the official images read it only when the data directory is empty. `--generated` does both:

```sh
yoho secrets rotate --generated POSTGRES_PASSWORD --dry-run   # strategy, user, Services that restart
yoho secrets rotate --generated POSTGRES_PASSWORD
yoho secrets rotate --generated MYSQL_PASSWORD --generated MYSQL_ROOT_PASSWORD
```

Under the deploy lock, for each key:

1. Yoho draws a new value with the key's generator (`password32`, `hex64`, …) and writes it next to the old one as `generated/<NAME>.next` (0600). It is never printed.
2. It applies the value through the running container of the Service that generates the key:
   - **postgres**: `psql` over the container's local socket runs `ALTER ROLE "<user>" WITH PASSWORD 'SCRAM-SHA-256$4096:…'`, read from stdin. Yoho computes the SCRAM-SHA-256 verifier itself (random salt, 4096 iterations) and Postgres stores it as given, so the new password never reaches the server and a statement log (`log_statement = ddl` or `all`) records only the verifier. When the user is a superuser, statement logging is also off for that session. Then it logs in with the new password over the container's network address (the images trust the socket and loopback, so only that login checks it). The role then authenticates with SCRAM even where `pg_hba.conf` says `md5` (Postgres uses SCRAM for a SCRAM verifier); clients that only speak MD5 (libpq before 10, very old drivers) cannot log in, and `password_encryption = md5` is ignored for this value.
   - **mysql**: the `mysql` or `mariadb` client authenticates with the current password from a temporary 0600 option file inside the container. It first reads `version`, `general_log`, `slow_query_log` and `log_raw`, tries `SET SESSION sql_log_off = 1` (needs SUPER or SYSTEM_VARIABLES_ADMIN; usually only root has it) and on MariaDB reads the account's plugin with `SHOW CREATE USER CURRENT_USER()`. The change runs with `sql_log_off` on when allowed. For root it runs `ALTER USER` for `root@localhost` and `root@%`; for other users, `ALTER USER CURRENT_USER()` (MySQL) or `SET PASSWORD` (MariaDB). Then it logs in with the new password.
     The plaintext stays out of the general log (which `yoho app logs` shows when it goes to stdout): MariaDB logs statements verbatim, so for a `mysql_native_password` account (the official image's default) Yoho sends the client-computed hash (`SET PASSWORD = '*<hex>'`, `IDENTIFIED BY PASSWORD`). MySQL replaces the password in `ALTER USER` with `<secret>` in its logs, unless `log_raw` is on. A MariaDB account with another plugin (ed25519, …) while the general log is on without `sql_log_off`, or the slow query log is on, and MySQL with `log_raw` while the general log is on without `sql_log_off`, are refused before the password changes: turn those logs off, grant the privilege, or change the password by hand.
   - **command**: `docker exec -i <container> <command…>` with two lines on stdin: the current value, then the new one. Exit 0 means applied. It must be idempotent.
   - **none**: nothing to apply (a signing key, say). Sessions or tokens signed with the old value stop working.
3. It renames `<NAME>.next` over `<NAME>`, appends a line with the new value's fingerprint (never the value) to `rotations.jsonl` in the App directory, and restarts every Service that uses the key exactly like `yoho secrets rotate`: proxied Services through the Proxy, Stateful Services stop-first. The Release is recorded again with the new fingerprints.

Values reach the Server over stdin or the SSH env file, never in argv or a script, and database errors are scrubbed of both values. Existing database connections keep working until their Service restarts; new ones made with the old value fail, so the restart follows right away.

**Strategy detection.** `POSTGRES_PASSWORD` uses `postgres` with `POSTGRES_USER` (default `postgres`). `MYSQL_ROOT_PASSWORD` and `MARIADB_ROOT_PASSWORD` use `mysql` as root; `MYSQL_PASSWORD` and `MARIADB_PASSWORD` use `mysql` as `MYSQL_USER` / `MARIADB_USER`. MariaDB is recognized by its image or the `MARIADB_` prefix. Other keys of a Stateful Service, or of a Postgres/MySQL image, need an explicit strategy; other keys elsewhere use `none`. Set it on the generating Service:

```yaml
services:
  db:
    image: postgres:17
    x-yoho:
      stateful: true
      generate: {POSTGRES_PASSWORD: password32, REPLICATOR_PASSWORD: password32}
      rotate:
        REPLICATOR_PASSWORD: {strategy: postgres, user: replicator}
  queue:
    image: rabbitmq:4
    x-yoho:
      stateful: true
      generate: {RABBITMQ_PASS: password32}
      rotate:
        RABBITMQ_PASS:
          strategy: command
          # stdin: current value, then new value
          command: ["sh", "-c", "read -r old; read -r new; rabbitmqctl change_password app \"$new\""]
```

A `command` gets the values on stdin only; what it does with them inside the container (here `rabbitmqctl` takes the password as an argument) is up to it.

**Failure and re-runs.** If the database step fails, `<NAME>` keeps the old value, nothing restarts for that key, and the hint says so. `<NAME>.next` stays: the next `--generated` run reuses it, and the strategy authenticates with the old value first and the pending one second, so a run that stopped after the database step finishes cleanly. Once the database took the new value, a failed login check with it (Postgres over the network, MySQL) is a warning, not a failure: `<NAME>` is swapped and Services restart anyway, so the database and `<NAME>` never disagree. If the swap itself fails, the hint says the database already uses the pending value in `<NAME>.next`: re-run `yoho secrets rotate --generated <NAME>` to finish, and do not `yoho deploy` until then. With several keys, they rotate in order and a failure stops the rest; the Services that use the keys already rotated still restart, and rotate fails naming which keys rotated and which still use the old value. If the restart fails after the swap (or the run is killed), the database already uses the new value: run the same `yoho secrets rotate --generated <NAME>` again, or `yoho secrets rotate` without `--generated`, to restart the remaining Services. The swap leaves `<NAME>.swapped` (the new value's fingerprint) until the restart finishes, so the re-run finishes that restart instead of drawing another value; once the new value is rolled out, `--generated` rotates to yet another value.

**Pending rotations.** While a `<NAME>.next` exists, the database may already use it while `<NAME>` still holds the old value. `yoho deploy`, `yoho apply`, `yoho rollback` and a plain `yoho secrets rotate` refuse (`yoho plan` warns) with ``pending rotation of <NAME> (generated/<NAME>.next): the database may already use the new value; run `yoho secrets rotate --generated <NAME>` to finish it``. A killed rotate also leaves the Destination lock: run `yoho lock release --force`, then re-run. A teammate on a Yoho older than v0.6.0 ignores `generated/*.next`, so their deploy during a pending rotation starts Services with the old value while the database may already use the new one, an authentication outage: upgrade the CLI together.

**Limits.** Compose runtime only; Swarm is refused. The database Service must be running. A rollback to a Release from before the rotation keeps the rotated value, which the database uses. Only `yoho rollback VERSION --with-release-secrets` starts its Services with the old value, which the database no longer accepts: it warns per key (``KEY was rotated after this Release (<time>); …``, from `rotations.jsonl`), and you rotate again after the rollback with `yoho secrets rotate --generated KEY`. Restoring a Backup made before the rotation depends on how the database is backed up. With `volumes`, the data directory holds the roles, so the old generated value is restored with it and they match. With `dump` only (`pg_dump`, `mysqldump` without grants), the dump has no roles: the database keeps the rotated password, so `yoho backup restore` keeps the current generated value for that Service's keys and says so. With scoped access, run `yoho access secrets push` afterwards.

## Rollback keeps the current secrets

`yoho rollback VERSION` rolls back images and the compiled compose, not secrets (like Kamal). Its Services read the current Release's values and the generated values as they are now, so a rotated or leaked secret never comes back. Yoho writes a new secrets generation in the old Release's layout (which Service reads which name, as files or `secrets_as_env`), points the Release's `compose.yaml` at it, keeps the file as first deployed as `compose.release.yaml`, and records the Release again with `rollback_secrets: current`, `secrets_generation` (the new one) and `secrets_from` (the generation the values came from). Swarm re-points the stack file at the current Release's Swarm secrets the same way.

A rollback refuses, before it changes anything, when the old Release reads a secret the current Release does not have:

```
Release 3f2a… reads secrets the current Release 9c1d… does not have: web/LEGACY_TOKEN
hint: deploy a Version that declares these secrets (their values must be in .yoho/secrets), or roll back with --with-release-secrets …
```

A value is matched by Service and name, a generated value by its name in `generated/`, and a renamed Service by the same provider reference. `yoho rollback VERSION --with-release-secrets` runs the Release's own generation instead, as first deployed, with a loud warning: every value rotated since (a replaced API key, a leaked password, a rotated database password) comes back until the next deploy. Retention keeps that generation while the Release is retained. Rollback does not resolve local secrets.

## Rotate everything after an incident

When a Server, a laptop, a CI token or a provider account may be compromised, assume every value it could read is known. From a checkout of the deployed Version:

1. **Contain.** Revoke suspect provider sessions or service-account tokens. Authorize replacement SSH keys with `setup.authorized_keys`, then `yoho setup`; setup adds keys but does not remove old ones, so remove the revoked key from the deploy user's `~/.ssh/authorized_keys` on every Server. If setup first connected as root, also check `/root/.ssh/authorized_keys` and remove revoked keys. Rotate Builder SSH keys too. Revoke Gate Member keys with `yoho access key rm MEMBER` and App permissions with `yoho access rm MEMBER`. If the Server itself is suspect, build a new one instead of cleaning it.
2. **List.** `yoho secrets list` shows every key and its source. Generated keys are under `x-yoho.generate` in the compose files. Include Backup Target credentials, `CF_*` tunnel tokens, registry credentials and `builder.secrets`.
3. **Rotate provider values.** Create new values in 1Password or Bitwarden (new API keys at each issuer), then `yoho secrets rotate --dry-run` and `yoho secrets rotate`. Rotation does not rebuild images; if a rotated `builder.secrets` value is baked into an image, run `yoho deploy` to rebuild. If used, rotate the Cloudflare Tunnel token, update its secret source, and run `yoho tunnel up`. Keep the Backup repository's `password_secret`: replacing its source does not re-key old Backups. Reinstall credential-bearing Scheduled Jobs with `yoho schedule install`; check Backup access with `yoho backup check`.
4. **Rotate generated values.** `yoho secrets rotate --generated KEY` for each key from step 2, root and superuser passwords last.
5. **Revoke the old values** at each issuer once the new ones are live, and run `yoho access secrets push` if Members deploy.
6. **Check.** `yoho app ps`, `yoho releases`, and `yoho audit`; `rotations.jsonl` in the App directory records each generated rotation by fingerprint. Old Backups still hold old values: keep them encrypted, or prune them.

## Giving a Service its secrets

```yaml
x-yoho:
  secrets:
    - DATABASE_PASSWORD:POSTGRES_PASSWORD   # container name <- secret key
    - SECRET_KEY_BASE
  secrets_as_env: false                     # true: env_file instead of files
```

Each Service receives only what it declares. Apps read files via the `*_FILE` convention (`POSTGRES_PASSWORD_FILE=/run/secrets/POSTGRES_PASSWORD`).

## Rules

- Do not use `${VAR}` interpolation for secrets in compose: it leaks through `docker compose config`. `yoho config check` rejects it.
- Provider failures are classified without showing provider output: `secrets_approval_timeout` (the approval prompt of `op`, `bw` or `bws` went unanswered or was denied, or a `command` provider exited 75), `secrets_not_signed_in`, `secrets_item_not_found`. The `--json` `step_fail` event carries the `code` and a distinct `hint`; other failures keep the redacted command error.
- A provider call that runs longer than 3 s prints "waiting for secrets provider approval…" (`step_wait` in `--json`). On a terminal Yoho reruns an approval timeout once; set `--secrets-retries N` or `YOHO_SECRETS_RETRIES` to change it (default 0 without a terminal). The first provider value is read alone, so one approval covers the rest.
- 1Password: all `op` references of one account, from `providers` and from `$(op read op://…)` lines in `.yoho/secrets`, are read with a single `op inject`, so a command asks for approval at most once even where `op` authorizes each process separately (coding agents, scripts). A `$(...)` is batched only when it is exactly `op read [-n|--no-newline] [--account X] REF` with no `$`, pipes or other shell syntax, and not when the secrets files define an `OP_*` key. If the batch fails for a reason other than sign-in or approval, each reference is read on its own so the error names the key.
- `yoho secrets list` shows keys, sources, and lengths. `yoho secrets print KEY --reveal` works only on a terminal.
- `yoho secrets rotate [service...]` re-resolves values from the provider with no rebuild and no code change, shows which Services changed (names only), and does a zero-downtime rolling restart of only those Services. Stateful Services follow their normal stop-first rules. It records the current Release again, so its pre-rotation values are not kept. `--dry-run` lists what would restart. `--generated KEY` replaces a Server-generated value and applies it to the database first ([Rotating generated secrets](#rotating-generated-secrets)).
- A timed walkthrough of a secret change is in the [5-minute quickstart](quickstart.md#7-change-a-secret-no-web-downtime).
- Build-time secrets: list keys in `builder.secrets` and use `RUN --mount=type=secret`.
- Server-side jobs (Scheduled Backups) read stored files, not your password manager.
