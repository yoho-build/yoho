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

`yoho skill install` writes all bundled skills into the project for Claude Code and Codex. `yoho skill print [SKILL]` prints one.

## Prefer to do it yourself

See [Getting started](getting-started.md).
