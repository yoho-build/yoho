# Next release

<!-- Renamed to docs/releases/vX.Y.Z.md when tagging; see README.md. -->
<!-- TODO(orchestrator): this release also adds `yoho tunnel route` (#67) and `x-yoho.check` (#68). Add their answers below (Gate install for checks through the Gate, how an older Gate treats `x-yoho.check`, restarts when a check is added) and their Changes entries. -->

## Upgrade notes

- **Gate (`yoho access install`): yes, for `yoho plan` with scoped access.** The Gate gains a read-only `plan` verb. Until the operator runs `yoho access install -d DESTINATION` with this release, a Member's `yoho plan` stops with `the Server's Gate cannot plan` and a hint to update the Gate. Deploys, rollbacks and the read verbs work with the previous Gate as before.
- **Gate settings (`yoho access secrets push`): no.** `settings.json`, the pushed secrets and `members.yml` keep their shape. `plan` uses the Member's existing `deploy` grant; no grant needs to change.
- **Older Gate and new compose: no change from #65.** The `plan` verb sends the same compose a deploy sends; nothing new is refused or ignored.
- **Running containers: no restarts.** Upgrading the CLI or the Gate touches no container. The operator plan only words its reasons differently (see Changes); what an operator or Gate deploy recreates is unchanged.
- **Proxy: unchanged** by #65 and #66.

## Changes

- `yoho plan` with scoped access runs through the Gate (#65). The Gate vets and hardens the compose exactly as a scoped deploy does and compares it with what runs, so a plan right after a Gate deploy shows no changes, instead of every non-proxied Service as `replace` with `config changed: cap_drop, cpus, mem_limit, pids_limit, pull_policy`. It pulls, locks and writes nothing, and needs the `deploy` grant.
- An operator `yoho plan` against a Release that the Gate deployed keeps showing those Services as `replace`, because an operator deploy really recreates them without the Gate's limits, but now says so: `Gate hardening removed by an operator deploy: …`, with a warning that points to `YOHO_ACCESS=scoped yoho plan`.
- Release notes with upgrade answers live in `docs/releases/` (#66); the release workflow publishes them and refuses a stable tag without them.
