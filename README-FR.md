<p align="center"><img src="docs/assets/logo.svg" alt="Yoho" width="120"></p>

# Yoho

[English](README.md) | [繁體中文](README-ZH-Hant.md) | [Français](README-FR.md) | [Deutsch](README-DE.md) | [Español](README-ES.md)

**Déployez vos applications Docker Compose sur vos propres serveurs comme Kamal déploie un conteneur : une commande, aucune interruption de service, aucune plateforme à maintenir.**

Yoho est un unique binaire Go statique. Votre `compose.yaml` reste la source de vérité ; Yoho ajoute un bloc `x-yoho` par service (Service) et un petit fichier `yoho.yml`. Il construit les images sur votre machine, les transfère par SSH (sans registre d’images), bascule le trafic via kamal-proxy, conserve les secrets dans votre gestionnaire de mots de passe et sauvegarde les volumes.

Statut : **alpha**. Des changements incompatibles sont à prévoir. Licence : MIT.

## Configuration avec votre agent IA

Aucune installation manuelle nécessaire. Votre agent de développement (Claude Code, Codex, Cursor, etc.) lit la compétence Yoho, installe le CLI, rédige la configuration et demande votre accord avant toute intervention sur un serveur (Server).

Dans le dépôt que vous souhaitez déployer, collez ce texte dans votre agent :

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

L’agent va :

1. Vous demander ce que vous souhaitez faire (nouveau déploiement, migration depuis Kamal, Coolify, Dokploy ou Ansible, ou gestion d’une application existante (App)) et quel serveur utiliser (`user@host`).
2. Installer `yoho` s’il est absent, après vous avoir demandé votre accord. Sans sudo.
3. Exécuter `yoho init` et rédiger `yoho.yml`, `x-yoho` et les références dans `.yoho/secrets`. Aucune valeur secrète dans le dépôt.
4. Exécuter `yoho config check`, puis `yoho setup --plan`. `yoho setup` étant interactif, vous l’exécutez vous-même (ou autorisez `-y`).
5. Exécuter `yoho plan` et vous présenter le résultat. Rien n’est déployé sans votre accord pour `yoho apply`.
6. Vérifier l’URL de santé, `yoho app ps` et `yoho releases`, et vous indiquer la commande de retour à une version précédente.

Gardez la compétence installée pour que les sessions suivantes connaissent Yoho sans ce texte :

```sh
npx skills add yoho-build/yoho    # CLI skills ; la compétence se trouve dans skills/yoho/SKILL.md
yoho skill install                # après l’installation du CLI ; --agent claude|codex|all
```

## Installation

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

Le programme d’installation télécharge depuis GitHub Releases le binaire correspondant au système d’exploitation et à l’architecture, vérifie le SHA-256 indiqué dans `checksums.txt` et l’installe dans `/usr/local/bin` si ce répertoire est accessible en écriture, sinon dans `~/.local/bin`. Il n’utilise pas sudo. Fixez une version avec `YOHO_VERSION=v0.1.0` ou choisissez un répertoire avec `YOHO_INSTALL_DIR`.

Pour vérifier, de manière facultative, la provenance signée du binaire, installez une version récente du CLI GitHub (`gh`) et utilisez `curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh`. Cette option exige une attestation correspondant à la version publiée et bloque l’installation si la vérification échoue ; les anciennes versions sans attestation restent utilisables en mode par défaut, qui vérifie uniquement la somme de contrôle. Voir [vérification de l’installation et authentification](docs/site/getting-started.md#signed-binary-provenance).

Depuis les sources (Go 1.26) : `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## Démarrage rapide en 60 secondes

```sh
yoho init                 # yoho.yml, .yoho/secrets, exemples de Hooks
$EDITOR yoho.yml compose.yaml
yoho config check         # valider la configuration, compose et les références aux secrets
yoho setup                # interactif : Docker, utilisateur de déploiement, pare-feu (Debian/Ubuntu)
yoho plan                 # afficher les changements prévus sur le serveur (lecture seule)
yoho apply                # confirmer, puis construire, transférer et basculer sans interruption après validation de la santé
```

Ensuite : `yoho app logs web -f`, `yoho app stats`, `yoho releases`, `yoho rollback <version>`.

## Fonctionnalités

- Natif Compose : `docker compose up` fonctionne toujours en local ; Yoho lit `x-yoho` et n’ignore aucun autre élément.
- Aucune interruption de service : bascule via kamal-proxy après validation de la santé et drainage des requêtes en cours, y compris derrière Cloudflare Tunnel.
- Sans registre d’images : construction locale (Apple `container` sur Apple silicon, Docker ailleurs), compilation croisée pour amd64, transfert des couches par SSH.
- Secrets transmis sous forme de fichiers par défaut : résolution locale depuis 1Password, Bitwarden ou toute commande ; transmission sous forme de fichiers ; génération de secrets sur le serveur. Dans ce mode, les valeurs restent absentes du dépôt, de l’image et de `docker inspect`. L’option `secrets_as_env: true`, à activer explicitement, transmet des variables d’environnement dont `docker inspect` peut révéler les valeurs.
- Les services avec état (Stateful Service) sont affectés à un serveur précis et ne subissent jamais de `down -v`.
- Sauvegardes (Backup ; restic, S3, B2, SFTP, rclone ou archives chiffrées) et restauration ; tâches planifiées (Scheduled Job) facultatives via des timers systemd.
- Cloudflare Tunnel, y compris les Quick Tunnels sans compte.
- Environnement d’exécution Compose (par défaut) ou Swarm.
- Points d’accroche (Hook) et format de fichier de secrets compatibles avec Kamal.
- `yoho setup` provisionne un serveur Debian/Ubuntu.
- Conçu pour l’IA : JSON Schema, sortie NDJSON avec `--json` et compétence intégrée pour les agents (`yoho skill install`).

## Comparaison

| | Kamal | ONCE | Coolify | Yoho |
|---|---|---|---|---|
| Unité de déploiement | conteneur par rôle | application unique | compose / applications | projet compose |
| Lieu d’exécution | CLI (Ruby) via SSH | sur le serveur | plateforme web + base de données | CLI (Go) via SSH |
| Sans interruption de service | kamal-proxy | kamal-proxy | partiellement | kamal-proxy |
| Registre d’images requis | non (depuis 2.8) | sans objet | oui | non |
| Secrets | `.kamal/secrets` | variables d’environnement | interface utilisateur, injection dans chaque conteneur | `.yoho/secrets`, fichiers par service |
| Sauvegardes | non | oui | base de données vers S3 | volumes + exports de données, restic/S3/B2/rclone |
| Provisionnement du serveur | Docker uniquement | programme d’installation | programme d’installation | `yoho setup` |
| Plusieurs serveurs | oui (rôles) | non | oui | environnement d’exécution Swarm ; rôles (Role) à venir |
| Plateforme à maintenir | non | service | oui | non |

## Exemple de configuration

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

Un service exposé via le proxy (Proxy) a besoin de ce `healthcheck` compose (son absence est une erreur sous swarm et un avertissement sous compose). Cet exemple exécute `wget` ; l’image doit donc contenir `wget`. Si vous utilisez `curl`, modifiez la commande de vérification de santé en conséquence.

Voir `examples/basic` pour un fichier plus complet.

## Commandes

| Commande | Objectif |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | créer et valider la configuration |
| `setup` | provisionner un serveur |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | examen et cycle de vie des versions déployées (Release) |
| `app ps\|logs\|stats\|exec` | inspecter l’application en cours d’exécution |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down` | proxy et Cloudflare Tunnel |
| `secrets list\|print` | inspecter les secrets |
| `backup run\|list\|restore`, `schedule install\|status\|remove` | sauvegardes et tâches planifiées |
| `swarm init\|join\|status` | environnement d’exécution Swarm |
| `builder status\|setup` | machine de construction locale (Builder) |
| `skill install\|print [SKILL]` | compétences pour les agents |

Options globales : `-c/--config`, `-d/--destination` (par défaut : `$YOHO_DESTINATION`, puis `production`, puis l’unique environnement de déploiement (Destination)), `--json`, `-v/--verbose`.

## Documentation

Les sources se trouvent dans `docs/site` (VitePress). Depuis la racine du dépôt, exécutez `npm ci` pour installer les dépendances, puis `npm run docs:dev` pour lancer le serveur de développement local. Conception : `VISION.md`, `GLOSSARY.md`, `docs/adr/`.
