# Object storage

Two choices for S3-compatible buckets:

- **An external store** (Cloudflare R2, Backblaze B2, AWS S3) for large or fast-growing data, data you serve to browsers directly, or data that must survive losing the Server. The provider keeps the copies; Yoho only delivers the credentials.
- **RustFS on the Server** (`yoho init --template rustfs`) for small, self-hosted buckets: uploads of an internal tool, attachments of a small App, a staging copy. It is one more Stateful Service with a volume and a Backup.

Prefer R2 (or B2, S3) when the bucket is larger than the disk you want to back up every night (as a rule of thumb, more than about 50 GB), when objects are public downloads (R2 has no egress fees), or when one Server going away must not take the data with it. A volume Backup copies the whole bucket every run; an external store does not need one.

## RustFS template

```sh
yoho init --template rustfs            # or --template postgres,rustfs
yoho init --list-templates
```

It writes (or appends to `compose.yaml`) a `rustfs` Service:

- image `rustfs/rustfs:1.0.4` (pinned; bump it yourself after reading the release notes),
- `x-yoho.stateful: true` and a named volume `rustfsdata` at `/data`,
- `x-yoho.generate` for `RUSTFS_ACCESS_KEY` and `RUSTFS_SECRET_KEY` (`password32`, created once on the Server). Yoho mounts them as files and sets `RUSTFS_ACCESS_KEY_FILE` / `RUSTFS_SECRET_KEY_FILE`, which RustFS reads, so the keys are not in `docker inspect`,
- a healthcheck on `http://127.0.0.1:9000/health`,
- `x-yoho.backup.volumes: [rustfsdata]`,
- no published ports.

An existing compose file keeps its text; the Service and volume are appended. A Service or volume that already has the name is refused and nothing is written.

## Reaching it from Services

On the App's compose network the S3 endpoint is `http://rustfs:9000`. Use path-style addressing (`forcePathStyle: true`, `s3_use_path_style`, `AWS_S3_FORCE_PATH_STYLE`) and region `us-east-1`.

List the generated keys in the Service that connects:

```yaml
services:
  web:
    environment:
      S3_ENDPOINT: http://rustfs:9000
      S3_BUCKET: uploads
    depends_on:
      rustfs: {condition: service_healthy}
    x-yoho:
      secrets: [RUSTFS_ACCESS_KEY, RUSTFS_SECRET_KEY]   # /run/secrets/<NAME>, <NAME>_FILE
```

Most S3 SDKs read `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from the environment, not from files. Either read the files in your code, or map the names and opt into environment delivery for that Service (the values are then visible through `docker inspect`):

```yaml
    x-yoho:
      secrets: {AWS_ACCESS_KEY_ID: RUSTFS_ACCESS_KEY, AWS_SECRET_ACCESS_KEY: RUSTFS_SECRET_KEY}
      secrets_as_env: true
```

Create buckets with any S3 client from inside the network, for example:

```sh
ssh root@YOUR_SERVER
docker run --rm --network yoho-<app>-production_default --env-file s3.env amazon/aws-cli \
  --endpoint-url http://rustfs:9000 s3 mb s3://uploads
```

where `s3.env` (mode 0600) holds `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` copied from `<root>/apps/<app>/<destination>/generated/RUSTFS_*`. Delete it afterwards.

### Console

The web console listens on port 9001 inside the container and is not published. Do not add `ports:` for it on a public interface. Open it through SSH instead:

```sh
ssh root@YOUR_SERVER "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' yoho-<app>-production-rustfs-1"
ssh -N -L 9001:<that IP>:9001 root@YOUR_SERVER
# open http://127.0.0.1:9001 and sign in with the generated access and secret key
```

If you must publish it, bind it to loopback (`ports: ["127.0.0.1:9001:9001"]`) and still use an SSH tunnel.

## Backups

The template sets `backup.volumes: [rustfsdata]`. Without a `pre_backup` command the container is paused while the volume is copied, so the copy is consistent; S3 requests wait or fail during that pause. Restore stops the App, puts the volume back and starts it again. Generated keys are included only in encrypted Backup Targets (restic, or zip/7z with a password); see [Backups](backups.md).

For a bucket too large to copy every night, move it to an external store, or mirror it there with `rclone sync` from a Hook or your own job.

## External stores

Keep the credentials in `.yoho/secrets` as references and pass the endpoint as a plain variable:

```sh
# .yoho/secrets
S3_ACCESS_KEY_ID=$(op read op://Production/shop/S3_ACCESS_KEY_ID)
S3_SECRET_ACCESS_KEY=$(op read op://Production/shop/S3_SECRET_ACCESS_KEY)
```

```yaml
services:
  web:
    environment:
      S3_ENDPOINT: https://<account-id>.r2.cloudflarestorage.com   # B2: https://s3.<region>.backblazeb2.com
      S3_BUCKET: media
    x-yoho:
      secrets: [S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY]
```

Give each App (and each Destination) its own token scoped to its buckets.

## Moving data in

`rclone` copies between any two S3-compatible stores. Configure both remotes through environment variables so no key lands in argv or a file, copy with `--checksum`, then verify:

```sh
export RCLONE_CONFIG_SRC_TYPE=s3 RCLONE_CONFIG_SRC_PROVIDER=Other RCLONE_CONFIG_SRC_ENDPOINT=https://old.example.com
export RCLONE_CONFIG_SRC_ACCESS_KEY_ID="$(op read op://Production/shop/OLD_ACCESS_KEY_ID)"
export RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY="$(op read op://Production/shop/OLD_SECRET_ACCESS_KEY)"
# DST_*: the same for the new store; for RustFS use an SSH tunnel as for the console, port 9000
rclone mkdir dst:uploads
rclone sync src:uploads dst:uploads --checksum --progress
rclone check src:uploads dst:uploads
```

`rclone check` must report `0 differences found`. Objects uploaded in parts have no MD5, so rclone compares them by size; `rclone check --download` compares the bytes (it reads both sides). Run `sync` again just before the cutover to pick up late writes. The [Railway guide](migrating-from-railway.md#buckets) prints these commands for each bucket.

## RustFS caveats

- RustFS is younger than MinIO. Pin the tag and test an upgrade on a staging Destination first.
- A single volume has no erasure-coding parity: RustFS logs `automatic storage class has no parity` at start. The Backup is the redundancy.
- At start RustFS 1.0.4 makes an outbound HTTPS request (its update check against `version.rustfs.com`). A blocked or failing request only logs an error.
