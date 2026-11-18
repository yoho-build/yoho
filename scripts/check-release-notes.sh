#!/bin/sh
# Release notes check (docs/releases/README.md). Every notes file needs a
# non-empty "## Upgrade notes" section.
#
#   scripts/check-release-notes.sh            check docs/releases/v*.md and next.md
#   scripts/check-release-notes.sh --tag TAG  check the notes of TAG: a stable
#                                             tag (no "-") needs docs/releases/TAG.md,
#                                             a prerelease is checked when it has
#                                             one. Prints the file to publish.
#
# RELEASE_NOTES_DIR overrides docs/releases (tests).
set -eu

dir=${RELEASE_NOTES_DIR:-docs/releases}

check() {
	if ! grep -qx '## Upgrade notes' "$1"; then
		echo "$1: no '## Upgrade notes' section (see docs/releases/README.md)" >&2
		return 1
	fi
	# Some text under the heading, before the next "## " heading.
	if ! awk '$0 == "## Upgrade notes" { s = 1; next }
		s && /^## / { exit }
		s && NF && !/^<!--/ { found = 1 }
		END { exit !found }' "$1"; then
		echo "$1: the '## Upgrade notes' section is empty" >&2
		return 1
	fi
}

case ${1:-} in
--tag)
	[ $# -eq 2 ] || { echo "usage: $0 [--tag TAG]" >&2; exit 2; }
	tag=$2
	if ! printf '%s\n' "$tag" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?'; then
		echo "release tag $tag is not vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2
		exit 1
	fi
	f=$dir/$tag.md
	if [ ! -f "$f" ]; then
		case $tag in
		*-*) exit 0 ;; # a prerelease may go without notes
		esac
		echo "$f is missing: write the release notes before tagging (git mv $dir/next.md $f)" >&2
		exit 1
	fi
	check "$f"
	printf '%s\n' "$f"
	;;
'')
	n=0
	failed=0
	for f in "$dir"/v*.md "$dir"/next.md; do
		[ -f "$f" ] || continue
		n=$((n + 1))
		check "$f" || failed=1
	done
	if [ "$n" -eq 0 ]; then
		echo "no release notes under $dir" >&2
		exit 1
	fi
	exit "$failed"
	;;
*)
	echo "usage: $0 [--tag TAG]" >&2
	exit 2
	;;
esac
