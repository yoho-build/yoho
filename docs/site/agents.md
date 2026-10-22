# Set up with your AI agent

No manual install needed. Your coding agent (Claude Code, Codex, Cursor, ...) reads the Yoho skill, installs the CLI, writes the config, and asks before it touches any Server.

## The prompt

Paste this into your agent, in the repo you want to deploy:

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

Why curl: a fetch tool that summarizes the page can drop commands and flags. The skill is plain Markdown; the agent should read it verbatim.

## What the agent does

1. Asks what you want (new deploy, migrate from Kamal, Coolify, Dokploy or Ansible, or operate an existing App) and which Server (`user@host`). It confirms before running any remote command.
2. Checks `yoho version`. If missing, it asks, then runs the installer (`curl -fsSL https://yohobuild.com/install.sh | sh`) or `go install`. It never uses sudo.
3. Offers `yoho skill install` so the skill stays available in later sessions.
4. Runs `yoho init`, then edits `yoho.yml` and the `x-yoho` blocks using the config reference. Secrets are only references in `.yoho/secrets`, never values in the repo.
5. Runs `yoho config check`, then `yoho setup --plan`. `yoho setup` is interactive: you run it, or approve `-y`.
6. Runs `yoho plan` and shows it to you. Nothing changes on the Server until you approve `yoho apply`.
7. Verifies the health URL, `yoho app ps` and `yoho releases`, and reports the rollback command.

If a step fails two or three times, the agent stops and reports the error instead of guessing.

## Keep the skill installed

```sh
npx skills add yoho-build/yoho          # skills CLI; the skill lives at skills/yoho/SKILL.md
yoho skill install                      # after installing the CLI; --agent claude|codex|all
```

`yoho skill install` writes all bundled skills into the project for Claude Code and Codex. `yoho skill list` shows them with their descriptions, and `yoho skill print [SKILL]` prints one.

In a repo that is already on Yoho, install only what you need:

```sh
yoho skill install --only yoho                       # one skill; skips the migrate-* references
yoho skill install --exclude yoho-migrate-coolify    # everything else
yoho skill install --no-migrate-refs                 # all skills, without the yoho skill's migrate-* references
```

`--only` and `--exclude` are mutually exclusive. An existing layout is respected: when `.claude/skills` (or an existing `.agent/skills`) is a symlink to `.agents/skills`, or a skill folder is a symlink to the real one, the skill is written once and the other paths are reported as linked. Installing never deletes skills that are already present.

## Operating Servers safely

- Never run `docker buildx history` (including `ls`) on a Server. On Docker 28.x, after a build-cache prune, it can hit a nil-pointer panic in BuildKit (`llbsolver.filterHistoryEvents`). That crashes `dockerd` and stops every container on the Server. Containers under their own supervisor come back; the rest stay down until someone restarts them. Yoho never calls it. Use `yoho app ps`, `yoho app logs SERVICE -n N`, `docker ps` and `docker logs`. The bundled `yoho` and `yoho-migrate-coolify` skills say the same.
- `yoho config check`, `plan` and `deploy` warn about a compose Service whose `restart` is unset or `no`, because it stays down after a daemon crash or restart. Set `restart: unless-stopped`. One-shot jobs (a Service others wait on with `service_completed_successfully`, or one with `profiles`) and Services with `deploy.restart_policy` are not reported. The warning is advisory: Yoho cannot see a supervisor on a shared Server.

## Prefer to do it yourself

See [Getting started](getting-started.md).
