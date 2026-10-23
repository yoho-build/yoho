<p align="center"><img src="docs/assets/logo.svg" alt="Yoho" width="120"></p>

# Yoho

[English](README.md) | [繁體中文](README-ZH-Hant.md) | [Français](README-FR.md) | [Deutsch](README-DE.md) | [Español](README-ES.md)

**將 Docker Compose 應用程式部署到自己的伺服器，就像 Kamal 部署單一容器一樣：一個指令、零停機，不必另外維運平台。**

Yoho 是一個靜態 Go 執行檔。你的 `compose.yaml` 仍是唯一依據；Yoho 為每個服務（Service）加入一個 `x-yoho` 區塊，再加上一份小型的 `yoho.yml`。它會在你的電腦上建置映像檔、透過 SSH 傳送映像檔（不需要映像檔登錄庫）、透過 kamal-proxy 切換流量、將機密保存在密碼管理器中，並備份資料卷。

狀態：**beta**。請預期會有不相容的變更。授權：MIT。

## 透過 AI 代理設定

不必手動安裝。你的程式開發代理（Claude Code、Codex、Cursor 等）會讀取 Yoho 技能、安裝 CLI、撰寫設定，並在對任何伺服器（Server）進行操作前徵求你的同意。

在你要部署的儲存庫中，將以下內容貼給代理：

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

代理會：

1. 詢問你的需求（新的部署、從 Kamal、Coolify、Dokploy 或 Ansible 遷移，或操作現有的應用程式（App）），以及要使用哪一台伺服器（`user@host`）。
2. 若尚未安裝 `yoho`，先徵求同意再安裝。不使用 sudo。
3. 執行 `yoho init`，並撰寫 `yoho.yml`、`x-yoho` 和 `.yoho/secrets` 中的參照。機密值不會放入儲存庫。
4. 執行 `yoho config check`，再執行 `yoho setup --plan`。`yoho setup` 是互動式指令，因此由你執行（或核准使用 `-y`）。
5. 執行 `yoho plan` 並顯示結果。在你核准 `yoho apply` 前，不會部署任何內容。
6. 檢查健康狀態 URL、`yoho app ps` 和 `yoho releases`，並告訴你回復先前發佈版本的指令。

保留已安裝的技能，讓後續工作階段不必再貼提示詞也能了解 Yoho：

```sh
npx skills add yoho-build/yoho    # skills CLI；技能位於 skills/yoho/SKILL.md
yoho skill install                # 安裝 CLI 後使用；--agent claude|codex|all
```

## 安裝

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

安裝程式會從 GitHub Releases 下載符合目前作業系統與架構的執行檔，驗證 `checksums.txt` 中的 SHA-256，並在 `/usr/local/bin` 可寫入時安裝到該目錄，否則安裝到 `~/.local/bin`。它不使用 sudo。可用 `YOHO_VERSION=v0.1.0` 指定固定版本，或用 `YOHO_INSTALL_DIR` 選擇安裝目錄。

若要選擇性驗證執行檔的簽署來源證明，請安裝目前版本的 GitHub CLI（`gh`），並使用 `curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh`。這需要相符的發佈證明；驗證失敗時會停止安裝，而不會略過驗證。較舊、沒有證明的發佈版本，仍可使用預設的僅驗證校驗碼模式。請參閱[安裝驗證與身分驗證](docs/site/getting-started.md#signed-binary-provenance)。

從原始碼安裝（Go 1.26）：`go install github.com/yoho-build/yoho/cmd/yoho@latest`。

## 60 秒快速入門

```sh
yoho init                 # yoho.yml、.yoho/secrets、範例 Hook
$EDITOR yoho.yml compose.yaml
yoho config check         # 驗證設定、compose 與機密參照
yoho setup                # 互動式設定：Docker、部署使用者、防火牆（Debian/Ubuntu）
yoho plan                 # 顯示伺服器上將會有哪些變更（唯讀）
yoho apply                # 確認後建置、傳送，並在健康檢查通過後進行零停機切換
```

接著可使用 `yoho app logs web -f`、`yoho app stats`、`yoho releases`、`yoho rollback <version>`。

## 功能

- 原生支援 Compose：`docker compose up` 在本機仍可使用；Yoho 讀取 `x-yoho`，不會忽略其他內容。
- 零停機：透過 kamal-proxy，在健康檢查通過並讓既有請求完成後切換流量；也適用於 Cloudflare Tunnel 後方。
- 不需要映像檔登錄庫：本機建置（Apple silicon 上使用 Apple `container`，其他平台使用 Docker）、amd64 交叉建置，以及透過 SSH 推送映像檔層。
- 預設以檔案形式提供機密：在本機從 1Password、Bitwarden 或任何指令解析取得；以檔案形式傳送；也可在伺服器上產生機密。在此模式下，機密值不會出現在儲存庫、映像檔或 `docker inspect` 中。選擇啟用 `secrets_as_env: true` 則會以環境變數傳送機密，`docker inspect` 可能會顯示這些值。
- 有狀態服務（Stateful Service）固定在指定伺服器上，且絕不執行 `down -v`。
- 備份（Backup；restic、S3、B2、SFTP、rclone 或加密封存檔）與還原；可選擇啟用透過 systemd 計時器執行的排程工作（Scheduled Job）。
- Cloudflare Tunnel，包括不需要帳號的 Quick Tunnels。
- Compose（預設）或 Swarm 執行環境。
- 與 Kamal 相容的掛鉤（Hook）及機密檔案格式。
- `yoho setup` 可佈建 Debian/Ubuntu 伺服器。
- 為 AI 設計：JSON Schema、`--json` NDJSON 輸出，以及內建代理技能（`yoho skill install`）。

## 比較

| | Kamal | ONCE | Coolify | Yoho |
|---|---|---|---|---|
| 部署單位 | 每個角色一個容器 | 單一應用程式 | compose／應用程式 | compose 專案 |
| 執行位置 | 透過 SSH 執行的 CLI（Ruby） | 伺服器上 | 網頁平台 + 資料庫 | 透過 SSH 執行的 CLI（Go） |
| 零停機 | kamal-proxy | kamal-proxy | 部分支援 | kamal-proxy |
| 是否需要映像檔登錄庫 | 否（自 2.8 起） | 不適用 | 是 | 否 |
| 機密 | `.kamal/secrets` | 環境變數 | UI，注入每個容器 | `.yoho/secrets`，各服務獨立的檔案 |
| 備份 | 無 | 有 | 資料庫備份至 S3 | 資料卷 + 資料匯出，restic/S3/B2/rclone |
| 伺服器佈建 | 僅 Docker | 安裝程式 | 安裝程式 | `yoho setup` |
| 多伺服器 | 是（角色） | 否 | 是 | Swarm 執行環境；角色（Role）稍後提供 |
| 需維運平台 | 否 | 服務 | 是 | 否 |

## 設定範例

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

透過代理（Proxy）對外提供存取的服務需要上述 compose `healthcheck`（在 swarm 下缺少它會報錯，在 compose 下則會提出警告）。此範例執行 `wget`，因此映像檔必須包含 `wget`；若使用 `curl`，請相應修改健康檢查指令。

更完整的檔案請參閱 `examples/basic`。

## 指令

| 指令 | 用途 |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | 建立與驗證設定 |
| `setup` | 佈建伺服器 |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | 審閱與發佈（Release）生命週期 |
| `app ps\|logs\|stats\|exec` | 檢查執行中的應用程式 |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down` | 代理與 Cloudflare Tunnel |
| `secrets list\|print\|rotate` | 檢查或輪替機密 |
| `backup run\|list\|restore`, `schedule install\|status\|remove` | 備份與排程工作 |
| `swarm init\|join\|status` | Swarm 執行環境 |
| `builder status\|setup` | 本機建置機器（Builder） |
| `skill install\|print [SKILL]` | 代理技能 |

全域旗標：`-c/--config`、`-d/--destination`（預設依序為 `$YOHO_DESTINATION`、`production`、唯一的部署環境（Destination））、`--json`、`-v/--verbose`。

## 文件

原始檔位於 `docs/site`（VitePress）。請在儲存庫根目錄執行 `npm ci` 安裝相依套件，再以 `npm run docs:dev` 啟動本機開發伺服器。設計文件：`VISION.md`、`GLOSSARY.md`、`docs/adr/`。
