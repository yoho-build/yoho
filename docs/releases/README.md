# Release notes

Every release has notes in `docs/releases/<tag>.md`. The release workflow publishes that file as the top of the GitHub Release, above the generated changelog, and refuses a stable tag (no `-`) without it. A prerelease may go without notes; when it has a file, the file is checked and published.

Work for the next release adds its notes to `docs/releases/next.md` in the same pull request. To release:

```sh
git mv docs/releases/next.md docs/releases/vX.Y.Z.md
# edit the title, check every answer under "Upgrade notes"
git commit -m "Prepare vX.Y.Z"
git tag -a vX.Y.Z -m "vX.Y.Z: ..."
git push origin main vX.Y.Z
```

The next pull request that changes behavior starts a new `next.md` from the template below. `scripts/check-release-notes.sh` runs in CI on every `v*.md` and `next.md`, and in the release workflow with `--tag`.

## Rules

- `## Upgrade notes` is required and comes first. It answers each question below for this release, with "No" when nothing changes. Operators read it before they upgrade, so say what to run and when.
- `## Changes` is a short list a person can read. Name commands and keys; link issues. The generated list of commits follows it on the release page.
- Write facts you can point to in the diff (`git diff vPREVIOUS vX.Y.Z`). When unsure, check the code, not the commit message.

## Template

```markdown
# vX.Y.Z

## Upgrade notes

- **Gate (`yoho access install`):** Does the Gate on the Server need the new binary? For what (a new verb, a policy change)? What happens until it is updated?
- **Gate settings (`yoho access secrets push`):** Does `settings.json` or the pushed secrets generation change shape, or do new settings come from the Yoho file?
- **Older Gate and new compose:** Does a Gate of the previous release refuse (or silently ignore) something this release lets Members send? Which error do they see?
- **Running containers:** Do containers restart when the CLI or the Gate is upgraded? On the next deploy (operator or Gate), do any Services get recreated only because of this release?
- **Proxy:** Does the default Proxy image or its configuration change, so the next deploy or `yoho proxy upgrade` replaces it?

## Changes

- ...
```
