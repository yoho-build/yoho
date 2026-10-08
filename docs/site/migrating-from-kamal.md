# Migrating from Kamal

| Kamal | Yoho |
|---|---|
| `config/deploy.yml` | `yoho.yml` + `x-yoho` in `compose.yaml` |
| `-d dest`, `deploy.<dest>.yml` | `destinations:`, `-d dest` |
| roles | not yet; one Server per Destination |
| accessories | Services with `x-yoho.stateful: true` |
| `.kamal/secrets` | `.yoho/secrets` (same dotenv and `$(...)`) |
| `env.secret` | `x-yoho.secrets` (`NAME:KEY` aliases) |
| `env.clear` | compose `environment:` |
| kamal-proxy | kamal-proxy (`x-yoho.proxy`) |
| `.kamal/hooks` | `.yoho/hooks` (same names, `KAMAL_*` becomes `YOHO_*`) |
| `KAMAL_VERSION` in containers | `YOHO_VERSION` (plus `YOHO_APP`, `YOHO_DESTINATION`, `YOHO_SERVER`, `YOHO_SERVICE`) |

## Steps

1. Express roles and accessories as Services in `compose.yaml`; databases get `stateful: true`.
2. `yoho init`; set `app`, `servers.primary.ssh`, `destinations`. Map `builder.arch` to `builder.platforms`, `builder.remote` to `builder.location: remote`.
3. Proxy: `proxy.host` to `x-yoho.proxy.hosts`, `app_port` to `port`, `healthcheck.path` to `health_path`, `ssl` to `tls`.
4. Move `.kamal/secrets` to `.yoho/secrets`; list needed keys in `x-yoho.secrets`.
5. Copy hooks and rename `KAMAL_*` variables.
6. `yoho config check`, then deploy. Keep Kamal's proxy stopped or on other ports until Yoho is healthy; reuse accessory volumes after a dump.

The agent skill (`yoho skill install`) includes a longer guide an AI agent can follow.
