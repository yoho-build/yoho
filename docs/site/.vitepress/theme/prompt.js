// The agent prompt on the home page and /agents/. /skill.md is skills/yoho/SKILL.md (prepare.mjs).
export const agentPrompt = 'Read https://yohobuild.com/skill.md with curl and follow its "First-time setup" to deploy this repo with Yoho. Ask me before you install anything, edit files or connect to a server.'

export const installCommands = `curl -fsSL https://yohobuild.com/install.sh | sh
yoho init
yoho setup
yoho deploy`
