// Package safepath holds the POSIX sh building blocks Yoho uses to trust
// Server paths: the filesystem trust functions (moved from the Backup
// package), one-component-at-a-time creation, the trusted App directory link
// exception (app_dir), and the gate that coordinates App directory moves.
//
// Everything here is shell text. Callers concatenate it into one remote
// script and quote every value with remote.Quote.
package safepath

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// trust is deliberately POSIX sh; find's numeric -user and -perm work on
// GNU, BSD and BusyBox. Never repair permissions or ownership of existing paths.
// A root/user-owned sticky ancestor (e.g. /tmp) is allowed, but never the leaf.
// Checks reduce exposure to other UIDs, not races against root or our own UID.
//
// The only deploy-user link ever trusted is the canonical App directory
// <root>/apps/<app>/<destination> when the caller sets yoho_applink to that
// exact path (and yoho_appmark to the expected ownership marker). See
// yoho_linkok: the link must be owned by the deploy UID, point at a clean
// absolute path that passes these same checks, and that directory must hold
// a matching .yoho-app.json. Validation then continues on the physical
// target, and the ancestor walk continues above the canonical path.
const trust = `set -eu
umask 077
yoho_uid=$(id -u)
yoho_bad() { printf 'unsafe @LABEL@ path: %s\n' "$1" >&2; exit 1; }
yoho_owner() { [ -n "$(find "$1" -prune \( -user 0 -o -user "$yoho_uid" \) -print)" ] || yoho_bad "$1"; }
yoho_dir() {
 [ -d "$1" ] && [ ! -L "$1" ] || yoho_bad "$1"
 yoho_owner "$1"
 yoho_mode=$(find "$1" -prune \( -perm -0020 -o -perm -0002 \) -print) || yoho_bad "$1"
 if [ -n "$yoho_mode" ]; then
  [ "$2" = ancestor ] && [ -n "$(find "$1" -prune -perm -1000 -print)" ] || yoho_bad "$1"
 fi
}
yoho_linkok() {
 [ -L "$1" ] && [ -n "$(find "$1" -prune -user "$yoho_uid" -print)" ] || yoho_bad "$1"
 yoho_lt=$(readlink "$1") || yoho_bad "$1"
 case "$yoho_lt" in /*) ;; *) yoho_bad "$1";; esac
 case "$yoho_lt/" in //|*//*|*/./*|*/../*) yoho_bad "$1";; esac
 (yoho_path "$yoho_lt" leaf) || yoho_bad "$1"
 [ -d "$yoho_lt" ] && [ ! -L "$yoho_lt" ] || yoho_bad "$1"
 yoho_lm=$yoho_lt/.yoho-app.json
 [ -f "$yoho_lm" ] && [ ! -L "$yoho_lm" ] || yoho_bad "$1"
 [ -n "$(find "$yoho_lm" -prune -user "$yoho_uid" ! -perm -0020 ! -perm -0002 ! -links +1 -print)" ] || yoho_bad "$1"
 [ "$(cat "$yoho_lm")" = "${yoho_appmark:-}" ] || yoho_bad "$1"
}
yoho_path() {
 case "$1" in /*) ;; *) yoho_bad "$1";; esac
 yoho_p=$1
 while :; do
  if [ -e "$yoho_p" ] || [ -L "$yoho_p" ]; then
   yoho_kind=ancestor; [ "$yoho_p" != "$1" ] || yoho_kind=${2:-leaf}
   if [ -L "$yoho_p" ] && [ -n "${yoho_applink:-}" ] && [ "$yoho_p" = "$yoho_applink" ]; then
    yoho_linkok "$yoho_p"
   elif [ -L "$yoho_p" ]; then
    # Permit admin-owned filesystem aliases such as macOS /var, but never
    # a configured leaf or a deploy-user-created link. Check physical parents.
    [ "$yoho_kind" = ancestor ] && [ -n "$(find "$yoho_p" -prune -user 0 -print)" ] || yoho_bad "$yoho_p"
    yoho_real=$(cd -P "$yoho_p" && pwd -P) || yoho_bad "$yoho_p"
    (yoho_path "$yoho_real" ancestor) || yoho_bad "$yoho_p"
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
 yoho_t=$1
 if [ -n "${yoho_applink:-}" ] && [ "$1" = "$yoho_applink" ] && [ -L "$1" ]; then yoho_t=$(readlink "$1") || yoho_bad "$1"; fi
 [ -d "$yoho_t" ] && [ ! -L "$yoho_t" ] || yoho_bad "$1"
 [ -n "$(find "$yoho_t" -prune -user "$yoho_uid" ! -perm -0040 ! -perm -0020 ! -perm -0010 ! -perm -0004 ! -perm -0002 ! -perm -0001 -print)" ] || yoho_bad "$1"
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

// Script returns the trust functions. label names the kind of path in the
// refusal ("unsafe <label> path: ..."); Backups use "Backup".
func Script(label string) string {
	return strings.Replace(trust, "@LABEL@", label, 1)
}

// Creation defines yoho_make. Never mkdir -p through missing components under
// a shared sticky ancestor: another UID could insert a link after the initial
// check. Create one component exclusively, validate it, then descend. A
// collision is a refusal, not reuse.
const Creation = `yoho_make() {
 yoho_path "$1" "${2:-leaf}"
 if [ -d "$1" ]; then return; fi
 yoho_parent=${1%/*}; [ -n "$yoho_parent" ] || yoho_parent=/
 (yoho_make "$yoho_parent" ancestor) || yoho_bad "$1"
 mkdir -m 0700 "$1" || yoho_bad "$1"
 yoho_path "$1" "${2:-leaf}"
}
`

// MarkerName is the ownership marker Yoho writes into every App directory.
const MarkerName = ".yoho-app.json"

// Marker is the exact marker content for an App Destination (no newline).
func Marker(app, destination string) string {
	b, _ := json.Marshal(struct {
		App         string `json:"app"`
		Destination string `json:"destination"`
	}{app, destination})
	return string(b)
}

// AppLink returns shell assignments enabling the trusted App link exception
// for the canonical App directory of app/destination. Prepend it to a script
// built from Script; assignments before `set -eu` are fine.
func AppLink(app, destination string) string {
	return "yoho_applink=" + remote.Quote(release.AppDir(app, destination)) + "\n" +
		"yoho_appmark=" + remote.Quote(Marker(app, destination)) + "\n"
}

// GateTries is how many times a gate mkdir is attempted, one second apart.
// Normal holds last milliseconds; migrations hold the gate for a rename.
// A variable so tests do not sleep.
var GateTries = 5

// Gate defines the gate helpers. yoho_gate_take DIR takes a mkdir gate
// (retrying GateTries times) and records $yoho_gate_info in DIR/info.json;
// it returns 1 when the gate stays held. Taken gates are released on any
// exit by the EXIT trap, so a refusal never leaves one behind. A killed
// shell (SSH loss, SIGKILL) can; the held error names the gate to remove.
func Gate() string {
	return `yoho_gates=
yoho_gate_drop() {
 if [ -n "$yoho_gates" ]; then
  printf '%s\n' "$yoho_gates" | while IFS= read -r yoho_g; do
   [ -n "$yoho_g" ] || continue
   rm -f "$yoho_g/info.json"; rmdir "$yoho_g" 2>/dev/null || true
  done
 fi
 yoho_gates=
}
trap 'yoho_gate_drop' EXIT
trap 'exit 1' HUP INT TERM
yoho_gate_take() {
 yoho_n=0
 while ! mkdir "$1" 2>/dev/null; do
  yoho_n=$((yoho_n+1)); [ "$yoho_n" -lt ` + strconv.Itoa(max(GateTries, 1)) + ` ] || return 1
  sleep 1
 done
 yoho_gates="$yoho_gates
$1"
 printf '%s' "${yoho_gate_info:-}" > "$1/info.json" || true
}
`
}

// MetaDir is <root>/appdirs/<app>/d/<destination>: coordination files of one
// App Destination, outside the tree an App directory move renames.
func MetaDir(app, destination string) string {
	return path.Join(release.AppDirsDir(), app, "d", destination)
}

// GateDir is the Destination gate.
func GateDir(app, destination string) string { return path.Join(MetaDir(app, destination), "gate") }

// JournalPath is the migration journal.
func JournalPath(app, destination string) string {
	return path.Join(MetaDir(app, destination), "migrate.json")
}

// LinkTmpPath is the temporary symlink published over the canonical path.
func LinkTmpPath(app, destination string) string {
	return path.Join(MetaDir(app, destination), "link.tmp")
}

// AppGateDir is the App gate, held by migrations and by scans across all
// Destinations of the App (image retention).
func AppGateDir(app string) string { return path.Join(release.AppDirsDir(), app, "app.gate") }

// Gate refusal markers, printed as the first stdout line by LockTaker in
// report mode.
const (
	HeldLine    = "yoho-gate-held"
	JournalLine = "yoho-appdir-journal"
)

// LockTaker returns the prelude every writer of the canonical App directory
// runs: create the coordination directory, take the Destination gate, and
// refuse while a migration journal exists. The gate stays held until the
// script exits. info is the holder recorded in the gate (not secret).
//
// report=true prints HeldLine/JournalLine plus details on stdout and exits 0
// so the caller can build a typed error. report=false prints a message on
// stderr and exits 1.
func LockTaker(app, destination, info string, report bool) string {
	meta, gate, journal := MetaDir(app, destination), GateDir(app, destination), JournalPath(app, destination)
	held := "printf '%s\\t%s\\n' " + HeldLine + " " + remote.Quote(gate) + "; cat " + remote.Quote(gate+"/info.json") + " 2>/dev/null || true; exit 0"
	pending := "printf '%s\\t%s\\n' " + JournalLine + " " + remote.Quote(journal) + "; cat " + remote.Quote(journal) + " 2>/dev/null || true; exit 0"
	if !report {
		held = "printf 'App directory gate %s is held (an app_dir migration or another command is running); if it is stale, remove it on the Server\\n' " + remote.Quote(gate) + " >&2; exit 1"
		pending = "printf 'App directory migration in progress or interrupted (%s); rerun yoho deploy or yoho apply\\n' " + remote.Quote(journal) + " >&2; exit 1"
	}
	return "umask 077\n" + Gate() +
		"yoho_gate_info=" + remote.Quote(info) + "\n" +
		"mkdir -p " + remote.Quote(meta) + "\n" +
		"yoho_gate_take " + remote.Quote(gate) + " || { " + held + "; }\n" +
		"if [ -e " + remote.Quote(journal) + " ] || [ -L " + remote.Quote(journal) + " ]; then " + pending + "; fi\n"
}
