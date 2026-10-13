package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"

	"github.com/yoho-build/yoho/internal/remote"
)

// pathSafety is deliberately POSIX sh; find's numeric -user and -perm work on
// GNU, BSD and BusyBox. Never repair permissions or ownership of existing paths.
// A root/user-owned sticky ancestor (e.g. /tmp) is allowed, but never the leaf.
// Checks reduce exposure to other UIDs, not races against root or our own UID.
const pathSafety = `set -eu
umask 077
yoho_uid=$(id -u)
yoho_bad() { printf 'unsafe Backup path: %s\n' "$1" >&2; exit 1; }
yoho_owner() { [ -n "$(find "$1" -prune \( -user 0 -o -user "$yoho_uid" \) -print)" ] || yoho_bad "$1"; }
yoho_dir() {
 [ -d "$1" ] && [ ! -L "$1" ] || yoho_bad "$1"
 yoho_owner "$1"
 yoho_mode=$(find "$1" -prune \( -perm -0020 -o -perm -0002 \) -print) || yoho_bad "$1"
 if [ -n "$yoho_mode" ]; then
  [ "$2" = ancestor ] && [ -n "$(find "$1" -prune -perm -1000 -print)" ] || yoho_bad "$1"
 fi
}
yoho_path() {
 case "$1" in /*) ;; *) yoho_bad "$1";; esac
 yoho_p=$1
 while :; do
  if [ -e "$yoho_p" ] || [ -L "$yoho_p" ]; then
   yoho_kind=ancestor; [ "$yoho_p" != "$1" ] || yoho_kind=${2:-leaf}
   if [ -L "$yoho_p" ]; then
    # Permit admin-owned filesystem aliases such as macOS /var, but never
    # a configured leaf or a deploy-user-created link. Check physical parents.
    [ "$yoho_kind" = ancestor ] && [ -n "$(find "$yoho_p" -prune -user 0 -print)" ] || yoho_bad "$yoho_p"
    yoho_real=$(cd -P "$yoho_p" && pwd -P) || yoho_bad "$yoho_p"
    (yoho_path "$yoho_real" ancestor)
   else
    yoho_dir "$yoho_p" "$yoho_kind"
   fi
  fi
  [ "$yoho_p" != / ] || break
  yoho_p=${yoho_p%/*}; [ -n "$yoho_p" ] || yoho_p=/
 done
}
yoho_private() {
 yoho_path "$1"
 [ -d "$1" ] && [ ! -L "$1" ] || yoho_bad "$1"
 [ -n "$(find "$1" -prune -user "$yoho_uid" ! -perm -0040 ! -perm -0020 ! -perm -0010 ! -perm -0004 ! -perm -0002 ! -perm -0001 -print)" ] || yoho_bad "$1"
}
yoho_file() {
 yoho_path "${1%/*}"
 [ -f "$1" ] && [ ! -L "$1" ] || yoho_bad "$1"
 yoho_owner "$1"
 yoho_mode=$(find "$1" -prune \( -perm -0020 -o -perm -0002 -o -links +1 \) -print) || yoho_bad "$1"
 [ -z "$yoho_mode" ] || yoho_bad "$1"
}
yoho_secret() {
 yoho_file "$1"
 [ -n "$(find "$1" -prune -user "$yoho_uid" ! -perm -0040 ! -perm -0010 ! -perm -0004 ! -perm -0001 -print)" ] || yoho_bad "$1"
}
`

// Never mkdir -p through missing components under a shared sticky ancestor:
// another UID could insert a link after the initial check. Create one component
// exclusively, validate it, then descend. A collision is a refusal, not reuse.
const pathCreation = `yoho_make() {
 yoho_path "$1" "${2:-leaf}"
 if [ -d "$1" ]; then return; fi
 yoho_parent=${1%/*}; [ -n "$yoho_parent" ] || yoho_parent=/
 (yoho_make "$yoho_parent" ancestor)
 mkdir -m 0700 "$1" || yoho_bad "$1"
 yoho_path "$1" "${2:-leaf}"
}
`

func safeDir(dir string, private bool, create bool) string {
	fn := "yoho_path"
	s := pathSafety
	if create {
		s += pathCreation + "yoho_make " + remote.Quote(dir) + "\n"
	}
	if private {
		fn = "yoho_private"
	}
	return s + fn + " " + remote.Quote(dir) + "\n"
}

func safeFile(file string, secret bool) string {
	fn := "yoho_file"
	if secret {
		fn = "yoho_secret"
	}
	return pathSafety + fn + " " + remote.Quote(file) + "\n"
}

// current is the intentional Release symlink, unlike arbitrary Backup paths.
// Resolve it only inside this App's releases tree and validate the real file.
func safeCurrentFile(appDir, file string) string {
	current := path.Join(appDir, "current")
	return safeDir(appDir, true, false) +
		"yoho_owner " + remote.Quote(current) + "\n" +
		"yoho_app=$(cd -P " + remote.Quote(appDir) + " && pwd -P)\n" +
		"yoho_current=$(cd -P " + remote.Quote(current) + " && pwd -P)\n" +
		"case \"$yoho_current\" in \"$yoho_app/releases/\"*) ;; *) yoho_bad " + remote.Quote(current) + ";; esac\n" +
		"yoho_file \"$yoho_current/" + file + "\"\n"
}

func newStagingName(prefix string) (string, string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("Backup staging nonce: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:16]), hex.EncodeToString(b[16:]), nil
}

// The ownership marker is outside all extractor-writable mounts. Exclusive
// mkdir is the authority: a collision never gets a marker, even after SSH loss.
func createPrivateStaging(ctx context.Context, h remote.Host, base, dir, token string) error {
	return createStaging(ctx, h, base, dir, token, true)
}

// Publication Targets may be trusted 0755 directories; the staging child is
// still always exclusive/private. Spool callers require a private parent too.
func createStaging(ctx context.Context, h remote.Host, base, dir, token string, privateBase bool) error {
	return h.Run(ctx, remote.Cmd{Script: safeDir(base, privateBase, true) +
		"mkdir -m 0700 " + remote.Quote(dir) + "\n" +
		"(set -C; printf '%s' " + remote.Quote(token) + " > " + remote.Quote(path.Join(dir, checkOwnerMarker)) + ")\n"})
}

func removePrivateStaging(ctx context.Context, h remote.Host, dir, token string) error {
	qdir, marker := remote.Quote(dir), remote.Quote(path.Join(dir, checkOwnerMarker))
	script := pathSafety + "yoho_path " + remote.Quote(path.Dir(dir)) + "\n" +
		"if [ ! -e " + qdir + " ] && [ ! -L " + qdir + " ]; then exit 0; fi\n" +
		"yoho_private " + qdir + "\nyoho_secret " + marker + "\n" +
		"test \"$(cat " + marker + ")\" = " + remote.Quote(token) + "\n" +
		fmt.Sprintf("test \"$(wc -c < %s | tr -d '[:space:]')\" -eq %d\n", marker, len(token)) +
		"rm -rf " + qdir + "\n"
	return h.Run(ctx, remote.Cmd{Script: script})
}
