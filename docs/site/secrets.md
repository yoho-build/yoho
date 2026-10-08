# Secrets

Secrets are resolved on your machine at deploy time, written to the Server as versioned files (0444 in 0700 directories owned by the deploy user), and mounted via compose `secrets:` at `/run/secrets/<NAME>`. They stay out of the repo, the image, and `docker inspect`.

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
