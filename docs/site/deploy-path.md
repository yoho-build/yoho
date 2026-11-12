# What is in the deploy path

Keeping compute, data, and secrets with one provider can turn one outage into several blocked recovery steps. The question raised by [this incident discussion](https://x.com/theo/status/2113073058764530089) is useful for any App: can you still reach its Server, resolve replacement secrets, and deploy from a checkout you already have? Yoho keeps those steps under your control, but each dependency below can still fail.

## What a deploy talks to

| Dependency | When it is used |
| --- | --- |
| Your machine | Runs the Yoho binary, reads the App's configuration and checkout, and derives the Version locally. Local builds use Docker or Apple `container`; remote and Server builds use Docker on the Builder. See [Builds](builds.md). |
| Each Server over SSH | Runs Docker commands and receives App files, secrets, and images. A remote Builder also needs SSH; direct shipping needs SSH from that Builder to the Servers. |
| Your secret providers | `op`, `bw`, `bws`, and configured `command` providers run locally when secrets are resolved at deploy time. Secret-file `$(...)` commands run locally too. Generated secrets are created and stored on the Server. |
| Image registries | The Builder may fetch Dockerfile base images; Servers may pull compose `image:` references. Build commands can also depend on package repositories or other network services. |
| The Proxy image registry | The default image is `basecamp/kamal-proxy:v0.10.2` on Docker Hub. Proxy boot pulls it only if the image is absent on the Server; a running Proxy with matching configuration is reused. |
| TLS certificate issuance | When the Proxy terminates TLS, issuance and renewal need the ACME CA (Let's Encrypt by default); new [customer-domain certificates](custom-domains.md) also need the configured on-demand allow URL. Certificates persist in the `yoho-proxy-config` volume. |
| An optional App registry | Built images can ship over SSH without a registry. Registry transport pushes and pulls through the registry configured in the App's Yoho file. |
| Cloudflare, when a Tunnel is used | The connector carries traffic; SSH through Cloudflare Access also depends on it. Managed ingress updates use its API. Starting a connector may also need its image registry. |

There is no Yoho-hosted control plane, build farm, registry, or secret database in this path. By default, no resident Yoho process runs on the Server; Scheduled Jobs (systemd timers) and the Gate (forced SSH command) are opt-in. Services keep their delivered secret files on the Server; normal deploys resolve local secrets again. See [Secrets](secrets.md) and [Server setup](setup.md).

Provisioning is a separate dependency: `yoho setup` installs packages from OS and Docker repositories. Installing Scheduled Jobs can download a Yoho binary and verify its provenance; an available Server binary passed with `yoho schedule install --binary /path/to/yoho` avoids that download. Deploy also converges configured Scheduled Jobs, so their installation can fail after the App rollout succeeds.

## What an outage still blocks

| Vendor or dependency | What stops | What keeps working | Workaround |
| --- | --- | --- | --- |
| Password manager | Commands that resolve unavailable provider references or secret-file commands. | Already-running Services retain their files on the Server; they do not call the password manager to read them. | Keep an offline recovery copy and use literal values in `.yoho/secrets.<destination>`, or `KEY=$(cat /path/to/offline-secret)`. Cover every required key and remove unavailable commands from the common secrets file too; see the caveat below. |
| Git host | Fetching or pulling source from it; builds that fetch remote source can also fail. Release-binary downloads for Scheduled Jobs can fail separately. | Deploying from an existing local checkout does not fetch or pull git. The default Version is local `HEAD`, with `_uncommitted_<8 hex>` added for uncommitted content; `--version V` overrides it. | Keep a checkout and build inputs locally. For Scheduled Jobs, keep a Server binary available for `--binary`. |
| Base-image or Service-image registry, including Docker Hub | Builds needing uncached base images, compose pulls, and Proxy boot when its image is missing. | Running Services and the Proxy; cached Builder layers and images on the Server remain available. Cache availability does not guarantee an offline build. | `yoho deploy --skip-build --version V` reuses built images for that Version already on every Server. It does not suppress compose image pulls; check pull policies and image availability. |
| Optional App registry | Registry transport when it needs to push or pull built images. | Running Services and images already on the Servers. | Use SSH transport (`transport.mode: load`) for Docker images available locally, or build on the Server. |
| DNS / Cloudflare | Name resolution, new hostname setup, and traffic or SSH carried through an unavailable Tunnel. Managed ingress updates can fail. | Docker workloads can keep running. Direct SSH can still work when `servers.<name>.ssh` uses an IP and its SSH configuration does not route through the unavailable service. | Keep an independently reachable SSH path. SSH by IP does not restore public DNS or Tunnel traffic. |
| ACME CA / customer-domain allow endpoint | A CA outage blocks certificate issuance and renewal; an allow-endpoint outage blocks new customer-domain certificates. | Existing certificates in `yoho-proxy-config` keep serving TLS until expiry; cached certificates skip the allow check. | Preserve the Proxy volume and restore CA / allow-endpoint access before certificates expire; see [Customer-owned domains](custom-domains.md). |
| Server host provider | An unreachable or lost Server blocks deploys to it; losing the Server also stops its Services. | A local checkout and off-Server Backups, if kept elsewhere. | Restore onto another Server; see [High availability](high-availability.md) and [Losing a Server](backups.md#losing-a-server). Yoho does not provide automatic data failover. |

**Recovery-file caveat:** a file entry overrides a configured provider reference with the same key, so that reference is not read. But both `.yoho/secrets` and `.yoho/secrets.<destination>` are evaluated in order: a later literal does not skip an earlier `$(...)` command. Replace or remove those commands locally before relying on the recovery file. Keep that file out of git and store the offline copy securely. External APIs and databases your App calls have their own availability requirements too.

## Rotate everything and redeploy

Follow [Rotate everything after an incident](secrets.md#rotate-everything-after-an-incident).
With the password manager down, use the recovery file above, including its common-file caveat.
With a registry down, use `yoho deploy --skip-build --version V` only when that Version's built images already exist on every Server; compose image pulls can still fail.
Run `yoho deploy` only when code or build inputs changed, including rotated `builder.secrets` values baked into images; those changes need a rebuild, so do not use `--skip-build` for them.
After `yoho secrets rotate`, check with `yoho app ps`, `yoho releases`, and `yoho audit`.

## Related

- [Secrets](secrets.md)
- [Backups & restore](backups.md)
- [High availability & recovery](high-availability.md)
- [Migrating from Railway](migrating-from-railway.md)
