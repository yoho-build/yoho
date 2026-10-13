<p align="center"><img src="docs/assets/logo.svg" alt="Yoho" width="120"></p>

# Yoho

[English](README.md) · [繁體中文](README-ZH-Hant.md) · [Français](README-FR.md) · [Deutsch](README-DE.md) · [Español](README-ES.md)

**Despliega Apps de Docker Compose en tus propios servidores como Kamal despliega un contenedor: un comando, sin tiempo de inactividad y sin una plataforma que mantener en funcionamiento.**

Yoho es un único binario estático de Go. Tu `compose.yaml` sigue siendo la fuente de verdad; Yoho añade un bloque `x-yoho` por Service y un pequeño `yoho.yml`. Construye imágenes en tu máquina, las envía por SSH (sin necesidad de un registro), cambia el tráfico mediante kamal-proxy, mantiene los secretos en tu gestor de contraseñas y hace Backups de los volúmenes.

Estado: **alfa**. Cabe esperar cambios incompatibles. Licencia: MIT.

## Configuración con tu agente de IA

No hace falta instalarlo manualmente. Tu agente de programación (Claude Code, Codex, Cursor, ...) lee el skill de Yoho, instala la CLI, escribe la configuración y pregunta antes de actuar sobre cualquier Server.

Pega lo siguiente en tu agente, dentro del repositorio que quieras desplegar:

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

El agente hará lo siguiente:

1. Preguntará qué quieres hacer (un nuevo despliegue, migrar desde Kamal, Coolify, Dokploy o Ansible, u operar una App existente) y qué Server usar (`user@host`).
2. Instalará `yoho` si falta, después de preguntarte. Sin sudo.
3. Ejecutará `yoho init` y escribirá `yoho.yml`, `x-yoho` y las referencias de `.yoho/secrets`. Sin valores de secretos en el repositorio.
4. Ejecutará `yoho config check` y luego `yoho setup --plan`. `yoho setup` es interactivo, así que lo ejecutas tú (o autorizas `-y`).
5. Ejecutará `yoho plan` y te lo mostrará. No se despliega nada hasta que autorices `yoho apply`.
6. Verificará la URL de salud, `yoho app ps` y `yoho releases`, y te indicará el comando de rollback.

Mantén el skill instalado para que las sesiones posteriores conozcan Yoho sin este prompt:

```sh
npx skills add yoho-build/yoho    # skills CLI; the skill lives at skills/yoho/SKILL.md
yoho skill install                # after installing the CLI; --agent claude|codex|all
```

## Instalación

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

El instalador descarga de GitHub Releases el binario para este sistema operativo y arquitectura, verifica el SHA-256 de `checksums.txt` y lo instala en `/usr/local/bin` si se puede escribir en ese directorio; de lo contrario, en `~/.local/bin`. No utiliza sudo. Fija una versión con `YOHO_VERSION=v0.1.0` o elige un directorio con `YOHO_INSTALL_DIR`.

Para la verificación opcional de la procedencia firmada del binario, instala una GitHub CLI (`gh`) actual y usa `curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh`. Esto requiere una atestación de la versión correspondiente y aborta la instalación si la verificación falla; las versiones antiguas sin atestación siguen funcionando en el modo predeterminado, que solo comprueba la suma de verificación. Consulta [verificación de la instalación y autenticación](docs/site/getting-started.md#signed-binary-provenance).

Desde el código fuente (Go 1.26): `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## Inicio rápido en 60 segundos

```sh
yoho init                 # yoho.yml, .yoho/secrets, sample hooks
$EDITOR yoho.yml compose.yaml
yoho config check         # validate config, compose and secret refs
yoho setup                # interactive: Docker, deploy user, firewall (Debian/Ubuntu)
yoho plan                 # show what would change on the Server (read-only)
yoho apply                # confirm, then build, ship, health-gated zero-downtime cutover
```

Después: `yoho app logs web -f`, `yoho app stats`, `yoho releases`, `yoho rollback <version>`.

## Características

- Nativo de Compose: `docker compose up` sigue funcionando localmente; Yoho lee `x-yoho` y no ignora nada más.
- Sin tiempo de inactividad: cambio de tráfico mediante kamal-proxy condicionado a las comprobaciones de salud y con finalización de las solicitudes en curso, también detrás de Cloudflare Tunnel.
- Sin registro: construcción local (Apple `container` en Apple silicon, Docker en las demás plataformas), compilación cruzada para amd64 y envío de capas por SSH.
- Secretos basados en archivos de forma predeterminada: se resuelven localmente desde 1Password, Bitwarden o cualquier comando; se entregan como archivos; también se generan secretos en el Server. En este modo, los valores quedan fuera del repositorio, la imagen y `docker inspect`. La opción voluntaria `secrets_as_env: true` los entrega como variables de entorno, cuyos valores puede exponer `docker inspect`.
- Los Stateful Services están fijados a un Server y nunca se detienen con `down -v`.
- Backups (restic, S3, B2, SFTP, rclone o archivos cifrados) y restauración; Scheduled Jobs opcionales mediante temporizadores de systemd.
- Cloudflare Tunnel, incluidos Quick Tunnels sin cuenta.
- Compose (predeterminado) o Swarm como entorno de ejecución.
- Hooks y formato de archivo de secretos compatibles con Kamal.
- `yoho setup` prepara un Server Debian/Ubuntu.
- Nativo de IA: JSON Schema, salida NDJSON con `--json` y un skill de agente integrado (`yoho skill install`).

## Comparación

| | Kamal | ONCE | Coolify | Yoho |
|---|---|---|---|---|
| Unidad de despliegue | contenedor por Role | una sola App | Compose / Apps | proyecto Compose |
| Dónde se ejecuta | CLI (Ruby) por SSH | en el servidor | plataforma web + BD | CLI (Go) por SSH |
| Sin tiempo de inactividad | kamal-proxy | kamal-proxy | parcial | kamal-proxy |
| Registro necesario | no (desde 2.8) | no aplicable | sí | no |
| Secretos | `.kamal/secrets` | variables de entorno | UI, inyectados en todos los contenedores | `.yoho/secrets`, archivos por Service |
| Backups | no | sí | BD a S3 | volúmenes + dumps, restic/S3/B2/rclone |
| Preparación del Server | solo Docker | instalador | instalador | `yoho setup` |
| Varios Servers | sí (Roles) | no | sí | entorno de ejecución Swarm; Roles más adelante |
| Plataforma que mantener en funcionamiento | no | Service | sí | no |

## Ejemplo de configuración

```yaml
# yoho.yml
app: shop
servers:
  primary: { ssh: yoho@203.0.113.10 }
destinations:
  production: { servers: [primary] }
builder: { location: local, secrets: [NPM_TOKEN] }
backups:
  targets:
    offsite: { type: restic, repository: "s3:s3.amazonaws.com/my-backups/shop", password_secret: BACKUP_PASSWORD, env_secrets: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY], keep_last: 14 }
  jobs:
    nightly: { destination: production, target: offsite, schedule: "*-*-* 03:00:00" }
```

```yaml
# compose.yaml
services:
  web:
    build: .
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}
    x-yoho:
      proxy: { hosts: [shop.example.com], port: 3000 }
      secrets: [DATABASE_PASSWORD:POSTGRES_PASSWORD, SECRET_KEY_BASE]
      release_command: ["bin/rails", "db:migrate"]
  db:
    image: postgres:17
    environment: { POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD }
    volumes: [pgdata:/var/lib/postgresql/data]
    x-yoho:
      stateful: true
      generate: { POSTGRES_PASSWORD: password32 }
      backup: { dump: [pg_dump, -U, postgres, postgres], volumes: [pgdata] }
volumes: { pgdata: {} }
```

Un Service detrás del Proxy necesita ese `healthcheck` de Compose (un error con Swarm, una advertencia con Compose). Este ejemplo ejecuta `wget`, por lo que la imagen debe incluir `wget`; si usas `curl`, cambia el comando de comprobación de salud en consecuencia.

Consulta `examples/basic` para ver un archivo más completo.

## Comandos

| Comando | Propósito |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | crear y validar la configuración |
| `setup` | preparar un Server |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | revisión y ciclo de vida de los Releases |
| `app ps\|logs\|stats\|exec` | inspeccionar la App en ejecución |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down` | Proxy y Cloudflare Tunnel |
| `secrets list\|print` | inspeccionar secretos |
| `backup run\|list\|restore`, `schedule install\|status\|remove` | Backups y Scheduled Jobs |
| `swarm init\|join\|status` | entorno de ejecución Swarm |
| `builder status\|setup` | Builder local |
| `skill install\|print [SKILL]` | skills de agentes |

Opciones globales: `-c/--config`, `-d/--destination` (predeterminado: `$YOHO_DESTINATION`, después `production`, después la única Destination), `--json`, `-v/--verbose`.

## Documentación

El código fuente está en `docs/site` (VitePress). Desde la raíz del repositorio, ejecuta `npm ci` y `npm run docs:dev`. Diseño: [VISION.md](VISION.md), [GLOSSARY.md](GLOSSARY.md), [docs/adr/](docs/adr/).
