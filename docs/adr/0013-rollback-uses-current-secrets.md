# Rollback uses current secrets

Status: Accepted.

## Context

A Release records its secrets generation, and `yoho rollback VERSION` used to restart the old Release with that generation. After `yoho secrets rotate` (a leaked API key replaced in the password manager) or `yoho secrets rotate --generated` (a new database password), a rollback silently brought the old values back: a leaked key was live again, and Services started with a database password the database no longer accepted. Kamal rolls back images only; its env files keep the current secrets.

## Decision

1. **A rollback rolls back images and the compiled compose, not secrets.** Its Services read the current Release's values and the current `generated/` values. The runtime writes a new generation in the old Release's layout (Service, name, file or env delivery), points the Release's `compose.yaml` at it (Swarm: at the current Release's content-addressed Swarm secrets) and updates the `yoho.secrets` label so changed Services are recreated. No local secrets are resolved; rollback still needs no provider access.
2. **Missing values refuse.** When the old Release reads a secret the current Release does not deliver (matched by Service and name, a generated value by name, a renamed Service by the same provider reference), rollback refuses before changing anything and names the keys.
3. **`--with-release-secrets` opts back in**, with a loud warning and the per-key warnings for generated secrets rotated since. Scoped access (ADR 0011) has no such flag.
4. **The Release as first deployed is kept.** `compose.release.yaml` and `base_compose_sha256`, `base_secrets_generation`, `base_secrets` in `release.json` keep the original, which `--with-release-secrets` and the Gate's rollback check use; retention keeps that generation (and Swarm secrets) while the Release is retained. The record notes `rollback_secrets` (`current` or `release`) and `secrets_from`.

## Consequences

Rotating a secret is final unless the operator asks for the old value. A rollback across a secret that was added later needs a deploy that declares it, or `--with-release-secrets`. A Release's `compose.yaml` hash changes after a current-secrets rollback, as it does after `yoho secrets rotate`.
