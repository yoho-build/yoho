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

3. `x-yoho.generate`: created once on the Server, never in your password manager unless you back them up.

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
`tunnel up`, `secrets list`/`print`, and credential-bearing `schedule install`.
`config check` also resolves local secrets **only when this list is nonempty**;
without it, config inspection still runs no secret commands. This opt-in uses the
full existing resolution pipeline, not just listed keys: all applicable providers
and secret-file `$(...)` commands run, so an unrelated resolution failure can fail
`config check` even when every declared key has a literal value. Secret files are
trusted shell code; commands can depend on any previously defined key or provider
value (including indirect environment lookups), so Yoho does not infer a partial
shell dependency graph. Do not opt in expecting a credential-free structural check;
`config show` still does not resolve credentials.

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
- `yoho secrets list` shows keys, sources, and lengths. `yoho secrets print KEY --reveal` works only on a terminal.
- Build-time secrets: list keys in `builder.secrets` and use `RUN --mount=type=secret`.
- Server-side jobs (Scheduled Backups) read stored files, not your password manager.
