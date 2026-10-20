# setup-yoho

Installs a Yoho release binary for the runner's OS and architecture (linux or darwin, amd64 or arm64) and adds it to `PATH`. It needs no sudo, so it works on self-hosted macOS runners.

```yaml
- uses: yoho-build/yoho/.github/actions/setup-yoho@v0.2.0
  with:
    version: v0.2.0
```

| Input | Default | Meaning |
| --- | --- | --- |
| `version` | required | Release tag, or `latest`. |
| `verify-attestation` | `auto` | `auto` verifies the Sigstore build provenance when `gh` is installed. `true` fails without `gh`. `false` skips it. |
| `token` | `github.token` | Token for the release download and `gh attestation verify`. |

Output `path` is the absolute path of the binary. The action installs into `$RUNNER_TEMP/yoho-bin`, checks the sha256 against the release's `checksums.txt`, and prints `yoho version`.

The action runs `scripts/install.sh` from the same tag, so pin the action to a tag and set `version` to a release you trust. Pinning both to `v0.2.0` is the usual choice.
