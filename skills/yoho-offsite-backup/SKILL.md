---
name: yoho-offsite-backup
description: Set up encrypted off-Server Backups for a Yoho App with rclone (OneDrive, Google Drive, Dropbox, S3, R2, B2, SFTP), as password-protected 7z or zip archives or restic. Use when the user wants off-site, cloud or encrypted Backups, mentions rclone, OneDrive, Google Drive, `rclone_config_secret`, `password_secret`, a backup token that expired (`invalid_grant`, `couldn't fetch token`), `rclone config reconnect`, or wants to replace a hand-made rclone/7z backup container or cron script with `yoho backup`.
---

# Encrypted off-Server Backups with rclone

Playbook for sending an App's Backups off the Server. It assumes the `yoho` skill for general commands; `docs/site/backups.md` (yohobuild.com/backups) is the full reference.

Ground rules:

- Never print, log or commit secret values: not the rclone config, its token, nor the archive password. Read them with `$(...)` in `.yoho/secrets`, and check with `yoho secrets list` (keys only).
- Never pass a password or token on a command line you write yourself. Yoho sends secrets over SSH stdin.
- Keep a local Target as well. Off-site is a second copy, not the only one.
- Ask before changing the user's rclone remotes, cloud folders or Backup schedule.

## 1. Pick the format

| Target | Encryption | Restore needs | Use when |
| --- | --- | --- | --- |
| `type: archive`, `format: 7z` | AES-256, file names encrypted too | 7-Zip and the password | Default. One self-contained file per Backup, readable without Yoho. |
| `type: archive`, `format: zip` | AES-256 (never ZipCrypto) | 7-Zip or another AES zip tool | The user wants plain `.zip` files. |
| `type: restic`, `repository: rclone:<remote>:<path>` | restic repository key | restic and the password | Large data with deduplication and many snapshots. |

`format: tar.gz` cannot be encrypted. Yoho refuses it with `password_secret`.

## 2. Make the rclone remote on the user's machine

1. `rclone listremotes`. Reuse a remote if one already points at the right account; otherwise run `rclone config` and create one (for example `od`). Interactive steps need the user, so start the command in a terminal they can see and let them complete the browser login.
2. Test it: `rclone lsd od:`.
   - If it fails with `invalid_grant` or `couldn't fetch token`, the stored token has expired. Run `rclone config reconnect od:` and let the user log in.
   - For OneDrive, rclone then asks for the drive. If the drives it offers don't include the remote's old `drive_id` (`rclone config show od | grep drive_id`), the user signed in with a different Microsoft account. Ask which account and drive the Backups belong to before continuing.
3. Give each App Destination its own remote, or at least its own secret. OAuth providers (OneDrive, Google Drive, Dropbox) rotate refresh tokens. If two Destinations share one secret, each refresh breaks the other copy. `yoho config check` warns about this.

S3-style providers (S3, R2, B2) have no OAuth. Their keys can go in `env_secrets` (`RCLONE_CONFIG_<REMOTE>_ACCESS_KEY_ID` and so on) or in the same `rclone_config_secret` as below.

## 3. Wire it into Yoho

`.yoho/secrets` (values come from commands; nothing secret is written to the repo):

```sh
# rclone section for the remote: [od] header plus its fields, read at deploy time.
RCLONE_CONF=$(rclone config show od)
# Archive password. Prefer a password manager over a literal.
BACKUP_PASSWORD=$(op read op://Private/myapp-backup/password)
```

The password must also be stored somewhere the user can reach without the Server (1Password, Bitwarden). Without it the archives can't be opened.

`yoho.yml`:

```yaml
backups:
  targets:
    local:
      type: archive
      repository: /var/lib/yoho/backup-archives/myapp
      format: tar.gz
      keep_last: 14
    offsite:
      type: archive
      repository: "od:backups/myapp"   # rclone remote:path
      format: 7z
      password_secret: BACKUP_PASSWORD
      rclone_config_secret: RCLONE_CONF
      keep_last: 30
  jobs:
    nightly:
      destination: production
      target: local
      schedule: "*-*-* 04:40:00"
    offsite:
      destination: production
      target: offsite
      schedule: "*-*-* 05:10:00"
```

Services choose what is backed up through `x-yoho.backup` in compose (`dump`/`restore_dump` for databases, `volumes` for named volumes). See the `yoho` skill config reference.

No `scp` is needed. The first `yoho backup` command, `yoho schedule install` or `yoho deploy` writes the secret over SSH stdin to `<root>/backups/<app>/<destination>/.rclone/<KEY>/rclone.conf` on the Server: mode 0600, owned by the Scheduled Job user. rclone keeps refreshed tokens in that file. Yoho rewrites it only when the secret changes, so it never overwrites a refreshed token with an older one.

## 4. Verify

```bash
yoho config check
yoho deploy -d production          # installs the Scheduled Jobs
yoho backup run offsite
yoho backup list offsite           # the new .7z with size and time
yoho backup check offsite          # integrity, and "rclone config: seeded <date>"
yoho schedule status
```

From the user's machine, `rclone lsl od:backups/myapp` should show the same files. To prove the encryption, fetch one archive and run `7zz l <file>` (or `7z l <file>`) without a password. It must fail with "Wrong password?", and the file list must not be visible.

## 5. Restore

- `yoho backup restore <ID> -d <dest>` (see `yoho backup restore --help`) restores into the App. Rehearse on a non-production Destination first.
- Without Yoho: `rclone copy od:backups/myapp/<file>.7z .`, then `7zz x -p <file>.7z` (7-Zip prompts for the password). The archive holds `manifest.json`, the database dumps and volume tarballs.
- `yoho backup pull` copies only archives stored on the Server (`archive` Targets with a local path). For an rclone Target, use `rclone copy` as above.

## 6. When the token expires again

Symptoms: a Backup fails with `invalid_grant` or `couldn't fetch token`, and `yoho backup check` prints the re-authorization hint. Fix:

1. On the user's machine, `rclone config reconnect od:` (the user logs in).
2. Nothing else to edit when the secret reads `$(rclone config show od)`. Otherwise, update the secret.
3. `yoho schedule install -d production` (or any `yoho backup` command). The changed secret re-seeds the Server's file.
4. `yoho backup run offsite` to confirm.

## Migrating from a hand-made rclone/7z container

Common predecessors are an `rclone-backup` sidecar with a mounted `rclone.conf`, a cron script running `7z a -mhe=on -p...`, or a Kamal accessory.

1. Read the old setup's remote name, folder, schedule, format and retention without printing secrets. List env names only, and compare `rclone config show <remote> | sed 's/=.*/= …/'`.
2. Recreate it as the Target and job above, preferably in a new folder (`backups/<app>-yoho`), so old and new archives don't mix during the overlap.
3. Run `yoho backup run offsite` and `yoho backup check offsite`, then restore one archive into a scratch Destination.
4. Only after the user agrees, stop the old container or cron entry. Keep its existing archives until retention covers them.
