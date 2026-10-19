<p align="center"><img src="docs/assets/logo.svg" alt="Yoho" width="120"></p>

# Yoho

[English](README.md) · [繁體中文](README-ZH-Hant.md) · [Français](README-FR.md) · [Deutsch](README-DE.md) · [Español](README-ES.md)

**Docker-Compose-Apps auf deinen eigenen Servern bereitstellen, so wie Kamal einen einzelnen Container bereitstellt: ein Befehl, keine Ausfallzeit, keine Plattform zu betreiben.**

Yoho ist eine einzelne statische Go-Binärdatei. Deine `compose.yaml` bleibt die maßgebliche Quelle; Yoho ergänzt einen `x-yoho`-Block pro Service und eine kleine `yoho.yml`. Es baut Images auf deinem Rechner, überträgt sie über SSH (keine Registry erforderlich), schaltet den Datenverkehr über kamal-proxy um, bewahrt Secrets in deinem Passwortmanager auf und sichert Volumes.

Status: **Beta**. Rechne mit inkompatiblen Änderungen. Lizenz: MIT.

## Einrichtung mit deinem KI-Agenten

Keine manuelle Installation nötig. Dein Coding-Agent (Claude Code, Codex, Cursor, ...) liest den Yoho-Skill, installiert die CLI, schreibt die Konfiguration und fragt nach, bevor er auf einem Server Änderungen vornimmt.

Füge Folgendes in deinen Agenten ein, im Repository, das du bereitstellen möchtest:

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

Der Agent wird:

1. Fragen, was du möchtest (eine neue Bereitstellung, eine Migration von Kamal, Coolify, Dokploy oder Ansible oder den Betrieb einer bestehenden App) und welcher Server verwendet werden soll (`user@host`).
2. `yoho` nach Rückfrage installieren, falls es fehlt. Ohne sudo.
3. `yoho init` ausführen und `yoho.yml`, `x-yoho` und Referenzen in `.yoho/secrets` schreiben. Keine Secret-Werte im Repository.
4. `yoho config check` und anschließend `yoho setup --plan` ausführen. `yoho setup` ist interaktiv, daher führst du es selbst aus (oder genehmigst `-y`).
5. `yoho plan` ausführen und den Plan zeigen. Nichts wird bereitgestellt, bis du `yoho apply` genehmigst.
6. Die Health-URL, `yoho app ps` und `yoho releases` prüfen und dir den Rollback-Befehl nennen.

Lass den Skill installiert, damit spätere Sitzungen Yoho auch ohne diesen Prompt kennen:

```sh
npx skills add yoho-build/yoho    # skills CLI; the skill lives at skills/yoho/SKILL.md
yoho skill install                # after installing the CLI; --agent claude|codex|all
```

## Installation

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

Das Installationsskript lädt die Binärdatei für dieses Betriebssystem und diese Architektur von GitHub Releases herunter, prüft den SHA-256-Wert in `checksums.txt` und installiert sie unter `/usr/local/bin`, wenn dieses Verzeichnis beschreibbar ist, andernfalls unter `~/.local/bin`. Es verwendet kein sudo. Lege mit `YOHO_VERSION=v0.1.0` einen bestimmten Release fest oder wähle mit `YOHO_INSTALL_DIR` ein Verzeichnis.

Für die optionale Prüfung der signierten Herkunft der Binärdatei installiere eine aktuelle GitHub CLI (`gh`) und verwende `curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh`. Dies erfordert eine passende Release-Attestierung; schlägt die Prüfung fehl, wird die Installation abgebrochen. Ältere Releases ohne Attestierung funktionieren weiterhin im Standardmodus, der nur die Prüfsumme prüft. Siehe [Installationsprüfung und Authentifizierung](docs/site/getting-started.md#signed-binary-provenance).

Aus dem Quellcode (Go 1.26): `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## Schnellstart in 60 Sekunden

```sh
yoho init                 # yoho.yml, .yoho/secrets, sample hooks
$EDITOR yoho.yml compose.yaml
yoho config check         # validate config, compose and secret refs
yoho setup                # interactive: Docker, deploy user, firewall (Debian/Ubuntu)
yoho plan                 # show what would change on the Server (read-only)
yoho apply                # confirm, then build, ship, health-gated zero-downtime cutover
```

Danach: `yoho app logs web -f`, `yoho app stats`, `yoho releases`, `yoho rollback <version>`.

## Funktionen

- Compose-nativ: `docker compose up` funktioniert weiterhin lokal; Yoho liest `x-yoho` und ignoriert nichts anderes.
- Keine Ausfallzeit: Umschaltung über kamal-proxy nach erfolgreicher Health-Prüfung und mit Abwicklung laufender Anfragen, auch hinter Cloudflare Tunnel.
- Ohne Registry: lokaler Build (Apple `container` auf Apple silicon, ansonsten Docker), Cross-Builds für amd64, Übertragung von Layers über SSH.
- Standardmäßig dateibasierte Secrets: Sie werden lokal aus 1Password, Bitwarden oder einem beliebigen Befehl aufgelöst und als Dateien übergeben; Secrets werden auch auf dem Server erzeugt. In diesem Modus bleiben die Werte außerhalb des Repositorys, des Images und von `docker inspect`. Mit der optionalen Einstellung `secrets_as_env: true` werden sie als Umgebungsvariablen übergeben, deren Werte `docker inspect` offenlegen kann.
- Stateful Services sind fest einem Server zugeordnet und werden nie mit `down -v` heruntergefahren.
- Backups (restic, S3, B2, SFTP, rclone oder verschlüsselte Archive) und Wiederherstellung; optionale Scheduled Jobs über systemd-Timer.
- Cloudflare Tunnel, einschließlich Quick Tunnels ohne Konto.
- Compose (Standard) oder Swarm als Laufzeitumgebung.
- Mit Kamal kompatible Hooks und Secrets-Dateiformat.
- `yoho setup` richtet einen Debian-/Ubuntu-Server ein.
- KI-nativ: JSON Schema, NDJSON-Ausgabe mit `--json` und ein integrierter Agent-Skill (`yoho skill install`).

## Vergleich

| | Kamal | ONCE | Coolify | Yoho |
|---|---|---|---|---|
| Bereitstellungseinheit | Container pro Role | einzelne App | Compose / Apps | Compose-Projekt |
| Ausführung | CLI (Ruby) über SSH | auf dem Server | Webplattform + DB | CLI (Go) über SSH |
| Keine Ausfallzeit | kamal-proxy | kamal-proxy | teilweise | kamal-proxy |
| Registry erforderlich | nein (seit 2.8) | nicht zutreffend | ja | nein |
| Secrets | `.kamal/secrets` | Umgebungsvariablen | UI, in jeden Container injiziert | `.yoho/secrets`, Dateien pro Service |
| Backups | nein | ja | DB nach S3 | Volumes + Dumps, restic/S3/B2/rclone |
| Server-Einrichtung | nur Docker | Installationsskript | Installationsskript | `yoho setup` |
| Mehrere Server | ja (Roles) | nein | ja | Swarm-Laufzeitumgebung; Roles später |
| Plattform zu betreiben | nein | Service | ja | nein |

## Konfigurationsbeispiel

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

Ein Service hinter dem Proxy benötigt diesen Compose-`healthcheck` (ein Fehler unter Swarm, eine Warnung unter Compose). Dieses Beispiel führt `wget` aus, daher muss das Image `wget` enthalten; wenn du `curl` verwendest, passe den Healthcheck-Befehl entsprechend an.

Unter `examples/basic` findest du eine ausführlichere Datei.

## Befehle

| Befehl | Zweck |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | Konfiguration erstellen und prüfen |
| `setup` | einen Server einrichten |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | Prüfung und Release-Lebenszyklus |
| `app ps\|logs\|stats\|exec` | die laufende App untersuchen |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down` | Proxy und Cloudflare Tunnel |
| `secrets list\|print` | Secrets untersuchen |
| `backup run\|list\|restore`, `schedule install\|status\|remove` | Backups und Scheduled Jobs |
| `swarm init\|join\|status` | Swarm-Laufzeitumgebung |
| `builder status\|setup` | lokaler Builder |
| `skill install\|print [SKILL]` | Agent-Skills |

Globale Optionen: `-c/--config`, `-d/--destination` (Standard: `$YOHO_DESTINATION`, dann `production`, dann die einzige Destination), `--json`, `-v/--verbose`.

## Dokumentation

Die Quelldateien liegen in `docs/site` (VitePress). Führe im Stammverzeichnis des Repositorys `npm ci` und `npm run docs:dev` aus. Design: [VISION.md](VISION.md), [GLOSSARY.md](GLOSSARY.md), [docs/adr/](docs/adr/).
