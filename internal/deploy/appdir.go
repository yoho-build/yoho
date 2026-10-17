package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// App directory (app_dir). The canonical <root>/apps/<app>/<destination>
// stays the path every reader uses. With app_dir set it is a symlink to
// app_dir, which Yoho owns completely and marks with .yoho-app.json.
// EnsureAppDir converges the canonical path to the configured state under
// the Destination gate, moving the directory (never copying) when app_dir
// changes. See docs/dev/plan-app-dir.md for the state and resume tables.

// AppDirError is a refusal to use or move an App directory. Msg names the
// paths; Hint says what to do.
type AppDirError struct {
	Msg  string
	Hint string
}

func (e *AppDirError) Error() string { return e.Msg }

// AppDirResult reports what EnsureAppDir did.
type AppDirResult struct {
	// Action: created, used, forward (canonical dir moved to app_dir),
	// back (app_dir moved to the canonical path), retarget (app_dir moved).
	Action string
	// Resumed is the kind of an interrupted migration that was finished
	// first (forward, back, retarget), or empty.
	Resumed string
	// Physical is the resolved App directory.
	Physical string
}

// appDirParams are the inputs of the ensure script. shim is test-only shell
// inserted after the helpers (to inject yoho_dev or a failing mv).
type appDirParams struct {
	app, dest, want string
	shim            string
}

// EnsureAppDir makes the canonical App directory of app/dest match appDir
// ("" means the default layout) on h, finishing an interrupted migration
// first. It holds the Destination gate for the whole run, and the App gate
// while it moves anything. It never takes the inner deploy lock; it refuses
// when the directory being moved contains lock/ or source.run-*.
func EnsureAppDir(ctx context.Context, h remote.Host, app, dest, appDir string) (*AppDirResult, error) {
	return runEnsureAppDir(ctx, h, appDirParams{app: app, dest: dest, want: appDir})
}

func runEnsureAppDir(ctx context.Context, h remote.Host, p appDirParams) (*AppDirResult, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: ensureAppDirScript(p)})
	if err != nil {
		return nil, appDirFailure(err)
	}
	return parseEnsureOutput(out)
}

func parseEnsureOutput(out string) (*AppDirResult, error) {
	first, rest, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if gate, ok := strings.CutPrefix(first, safepath.HeldLine+"\t"); ok {
		le := &LockedError{Dir: gate}
		_ = json.Unmarshal([]byte(strings.TrimSpace(rest)), &le.Holder)
		return nil, le
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 4 && f[0] == "ok" {
			return &AppDirResult{Action: f[1], Resumed: f[2], Physical: f[3]}, nil
		}
	}
	return nil, fmt.Errorf("App directory: unexpected output %q", out)
}

const appDirTrustHint = "every parent of the App directory must be owned by root or the deploy user and not group- or other-writable (sticky parents such as /tmp are allowed); the App directory itself must be a 0700 directory of the deploy user, not a symlink"

// appDirFailure turns the script's exit into an *AppDirError: exit 3 is an
// explicit refusal ("App directory: ...\nhint: ..."), a trust failure prints
// "unsafe App directory path: ...".
func appDirFailure(err error) error {
	var ee *remote.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	msg := strings.TrimSpace(ee.Stderr)
	if ee.Code == 3 {
		m, hint, _ := strings.Cut(msg, "\nhint: ")
		// A trust failure inside a refusal prints its own line first.
		if i := strings.LastIndex(m, "App directory: "); i > 0 {
			m = m[i:]
		}
		return &AppDirError{Msg: m, Hint: strings.TrimSpace(hint)}
	}
	if strings.Contains(msg, "unsafe App directory path") {
		return &AppDirError{Msg: msg, Hint: appDirTrustHint}
	}
	return err
}

func ensureAppDirScript(p appDirParams) string {
	canon := release.AppDir(p.app, p.dest)
	info, _ := json.Marshal(LockInfo{Performer: "yoho", Command: "app_dir", Time: time.Now().UTC()})
	vars := map[string]string{
		"canon":     canon,
		"want":      p.want,
		"meta":      safepath.MetaDir(p.app, p.dest),
		"gate":      safepath.GateDir(p.app, p.dest),
		"journal":   safepath.JournalPath(p.app, p.dest),
		"ltmp":      safepath.LinkTmpPath(p.app, p.dest),
		"appgate":   safepath.AppGateDir(p.app),
		"appparent": path.Dir(canon),
		"root":      release.Root,
		"heldline":  safepath.HeldLine,
	}
	var b strings.Builder
	b.WriteString(safepath.AppLink(p.app, p.dest))
	b.WriteString(safepath.Script("App directory"))
	b.WriteString(safepath.Creation)
	b.WriteString(safepath.Gate())
	for _, k := range []string{"canon", "want", "meta", "gate", "journal", "ltmp", "appgate", "appparent", "root", "heldline"} {
		b.WriteString(k + "=" + remote.Quote(vars[k]) + "\n")
	}
	b.WriteString("yoho_gate_info=" + remote.Quote(string(info)) + "\n")
	b.WriteString(appDirHelpers)
	b.WriteString(p.shim)
	b.WriteString(appDirMain)
	return b.String()
}

// appDirHelpers: yoho_dev prints a path's device (GNU/BusyBox stat -c, BSD
// stat -f). Renames use mv -T (GNU) so an existing directory is never merged
// into; elsewhere they fall back to plain mv only when the target is absent,
// and symlinks are replaced with mv -h (BSD).
const appDirHelpers = `yoho_dev() { stat -c %d "$1" 2>/dev/null || stat -f %d "$1"; }
yoho_mv_t() { mv -T "$1" "$2" 2>/dev/null; }
yoho_mv_h() { mv -h "$1" "$2" 2>/dev/null; }
yoho_refuse() { printf 'App directory: %s\n' "$1" >&2; [ -z "${2:-}" ] || printf 'hint: %s\n' "$2" >&2; exit 3; }
yoho_exists() { [ -e "$1" ] || [ -L "$1" ]; }
yoho_owned_link() { [ -L "$1" ] && [ -n "$(find "$1" -prune -user "$yoho_uid" -print)" ]; }
yoho_marked() {
 [ -d "$1" ] && [ ! -L "$1" ] || return 1
 yoho_m=$1/.yoho-app.json
 [ -f "$yoho_m" ] && [ ! -L "$yoho_m" ] || return 1
 [ -n "$(find "$yoho_m" -prune -user "$yoho_uid" ! -perm -0020 ! -perm -0002 ! -links +1 -print)" ] || return 1
 [ "$(cat "$yoho_m")" = "$yoho_appmark" ]
}
yoho_empty() {
 [ -d "$1" ] && [ ! -L "$1" ] || return 1
 yoho_ls=$(ls -A "$1") || yoho_refuse "cannot list $1" "app_dir must be readable by the deploy user"
 [ -z "$yoho_ls" ]
}
yoho_vacant() { ! yoho_exists "$1" || yoho_empty "$1"; }
yoho_parent() { yoho_pp=${1%/*}; [ -n "$yoho_pp" ] || yoho_pp=/; printf '%s\n' "$yoho_pp"; }
yoho_safe() {
 case "$1" in /*) ;; *) return 1;; esac
 case "$1/" in //|*//*|*/./*|*/../*) return 1;; esac
 case "$1" in *[!A-Za-z0-9_./-]*) return 1;; esac
}
yoho_mark() {
 if yoho_exists "$1/.yoho-app.json"; then
  yoho_marked "$1" || yoho_refuse "$1/.yoho-app.json is not this App's ownership marker (it belongs to another App or Destination, or is not a private regular file of the deploy user)" "use another app_dir, or fix the marker if this directory really is this App's"
  return 0
 fi
 (set -C; printf '%s' "$yoho_appmark" > "$1/.yoho-app.json") || yoho_refuse "cannot write $1/.yoho-app.json"
}
yoho_idle() {
 if yoho_exists "$1/lock"; then
  yoho_refuse "$1/lock exists: a deploy, rollback, Backup or Scheduled Job is running or crashed" "wait for it to finish; if it is stale, remove $1/lock on the Server and rerun"
 fi
 for yoho_f in "$1"/source.run-*; do
  if yoho_exists "$yoho_f"; then
   yoho_refuse "$yoho_f exists: a Server build is running or crashed" "wait for it to finish; if it is stale, remove $yoho_f on the Server and rerun"
  fi
 done
}
yoho_resolve() {
 yoho_rp=$1; yoho_rest=
 while ! yoho_exists "$yoho_rp"; do
  yoho_rest=/${yoho_rp##*/}$yoho_rest
  yoho_rp=$(yoho_parent "$yoho_rp")
 done
 yoho_rr=$(cd -P "$yoho_rp" 2>/dev/null && pwd -P) || yoho_refuse "cannot resolve $yoho_rp"
 [ "$yoho_rr" != / ] || yoho_rr=
 printf '%s%s\n' "$yoho_rr" "$yoho_rest"
}
# yoho_trusted runs a trust check in a subshell and, on failure, sets
# yoho_unsafe to the path yoho_bad reported.
yoho_trusted() {
 yoho_unsafe=$( ("$@") 2>&1 >/dev/null ) && return 0
 yoho_unsafe=${yoho_unsafe##*path: }
 [ -n "$yoho_unsafe" ] || yoho_unsafe=$2
 return 1
}
yoho_candidate() {
 yoho_safe "$1" || yoho_refuse "$1 is not a clean absolute path of safe characters"
 yoho_trusted yoho_path "$1" || yoho_refuse "$1 or one of its parents is not trusted (unsafe: $yoho_unsafe)" "every parent of app_dir must be owned by root or the deploy user and not group- or other-writable (sticky parents such as /tmp are allowed); app_dir itself must not be a symlink"
 yoho_c=$(yoho_resolve "$1")
 yoho_r=$(cd -P "$root" && pwd -P) || yoho_refuse "cannot resolve the Yoho root $root"
 case "$yoho_c/" in "$yoho_r/"*) yoho_refuse "$1 resolves to $yoho_c, inside the Yoho root $yoho_r" "choose an app_dir outside servers.<name>.root";; esac
 case "$yoho_r/" in "$yoho_c/"*) yoho_refuse "$1 resolves to $yoho_c, which contains the Yoho root $yoho_r" "choose an app_dir outside servers.<name>.root";; esac
 # Another App's directory (any Yoho file) owns everything below it; moving
 # it would carry this one along.
 yoho_a=$yoho_c
 while [ -n "$yoho_a" ] && [ "$yoho_a" != / ]; do
  yoho_a=${yoho_a%/*}
  if [ -e "${yoho_a:-}/.yoho-app.json" ] || [ -L "${yoho_a:-}/.yoho-app.json" ]; then
   yoho_refuse "$1 is inside the App directory ${yoho_a:-/} of another App or Destination" "choose an app_dir outside every App directory"
  fi
 done
}
yoho_private_dir() {
 yoho_trusted yoho_private "$1" && return 0
 if [ "$yoho_unsafe" = "$1" ]; then
  yoho_refuse "$1 must be a directory of the deploy user with mode 0700" "chmod 700 $1 (Yoho never repairs existing permissions)"
 fi
 yoho_refuse "$1 has an untrusted parent: $yoho_unsafe" "every parent of the App directory must be owned by root or the deploy user and not group- or other-writable (sticky parents such as /tmp are allowed); Yoho never repairs existing permissions"
}
yoho_samedev() {
 [ "$(yoho_dev "$1")" = "$(yoho_dev "$2")" ] || yoho_refuse "$1 and $2 are on different filesystems" "Yoho moves an App directory only within one filesystem (a rename keeps running containers' bind mounts); see Moving app_dir across filesystems in the configuration docs"
}
yoho_nested() {
 yoho_n1=$(yoho_resolve "$1"); yoho_n2=$(yoho_resolve "$2")
 for yoho_np in "$1|$2" "$yoho_n1|$yoho_n2"; do
  yoho_na=${yoho_np%%|*}; yoho_nb=${yoho_np#*|}
  case "$yoho_nb/" in "$yoho_na/"*) yoho_refuse "$2 is inside $1";; esac
  case "$yoho_na/" in "$yoho_nb/"*) yoho_refuse "$1 is inside $2";; esac
 done
}
yoho_app_gate() {
 [ -z "${yoho_have_appgate:-}" ] || return 0
 yoho_gate_take "$appgate" || { printf '%s\t%s\n' "$heldline" "$appgate"; cat "$appgate/info.json" 2>/dev/null || true; exit 0; }
 yoho_have_appgate=1
}
yoho_journal() {
 (set -C; printf '{"kind":"%s","from":"%s","to":"%s"}' "$1" "$2" "$3" > "$journal") || yoho_refuse "cannot write the migration journal $journal"
}
yoho_rename() {
 if yoho_mv_t "$1" "$2"; then return 0; fi
 if yoho_exists "$2"; then yoho_refuse "cannot move $1 to $2: $2 exists"; fi
 mv "$1" "$2"
}
yoho_publish() {
 if yoho_exists "$ltmp"; then
  yoho_owned_link "$ltmp" || yoho_refuse "$ltmp is not a symlink of the deploy user" "inspect it and remove it on the Server"
  [ "$(readlink "$ltmp")" = "$1" ] || rm -f "$ltmp"
 fi
 yoho_exists "$ltmp" || ln -s "$1" "$ltmp"
 if [ -d "$canon" ] && [ ! -L "$canon" ]; then yoho_refuse "$canon is a directory; cannot publish the App directory link"; fi
 if ! yoho_mv_t "$ltmp" "$canon"; then
  if yoho_exists "$canon"; then
   yoho_mv_h "$ltmp" "$canon" || yoho_refuse "cannot replace the symlink $canon (needs mv -T or mv -h)"
  else
   mv "$ltmp" "$canon"
  fi
 fi
 [ -L "$canon" ] && [ "$(readlink "$canon")" = "$1" ] || yoho_refuse "publishing $canon -> $1 failed"
}
yoho_observe() {
 for yoho_o in "$canon" "${yoho_jf:-}" "${yoho_jt:-}"; do
  [ -n "$yoho_o" ] || continue
  if [ -L "$yoho_o" ]; then yoho_s="symlink -> $(readlink "$yoho_o")"
  elif yoho_marked "$yoho_o"; then yoho_s="directory with this App's marker"
  elif yoho_empty "$yoho_o"; then yoho_s="empty directory"
  elif [ -d "$yoho_o" ]; then yoho_s="directory without this App's marker"
  elif yoho_exists "$yoho_o"; then yoho_s="not a directory"
  else yoho_s=missing; fi
  printf '%s: %s; ' "$yoho_o" "$yoho_s"
 done
}
yoho_unlisted() {
 yoho_refuse "interrupted app_dir migration in an unexpected state. Journal $journal: ${yoho_j:-?}. Observed: $(yoho_observe)" "inspect these paths, finish or undo the move by hand, then remove $journal and rerun"
}
yoho_do_forward() {
 if yoho_empty "$1"; then rmdir "$1"; fi
 yoho_rename "$canon" "$1"
 yoho_publish "$1"
 rm -f "$journal"
}
yoho_do_back() {
 yoho_rename "$1" "$canon"
 rm -f "$journal"
}
yoho_do_retarget() {
 if yoho_empty "$2"; then rmdir "$2"; fi
 yoho_rename "$1" "$2"
 yoho_publish "$2"
 rm -f "$journal"
}
yoho_resume_forward() {
 [ "$yoho_jf" = "$canon" ] || yoho_unlisted
 if [ -d "$canon" ] && [ ! -L "$canon" ]; then
  yoho_marked "$canon" && yoho_vacant "$yoho_jt" || yoho_unlisted
  yoho_candidate "$yoho_jt"
  yoho_make "$(yoho_parent "$yoho_jt")" ancestor
  yoho_idle "$canon"
  yoho_samedev "$canon" "$(yoho_parent "$yoho_jt")"
  yoho_samedev "$meta" "$appparent"
  yoho_do_forward "$yoho_jt"
 elif ! yoho_exists "$canon"; then
  yoho_marked "$yoho_jt" || yoho_unlisted
  yoho_candidate "$yoho_jt"
  yoho_samedev "$meta" "$appparent"
  yoho_publish "$yoho_jt"
  rm -f "$journal"
 elif [ -L "$canon" ] && [ "$(readlink "$canon")" = "$yoho_jt" ] && (yoho_linkok "$canon") 2>/dev/null; then
  if yoho_owned_link "$ltmp"; then rm -f "$ltmp"; fi
  rm -f "$journal"
 else
  yoho_unlisted
 fi
}
yoho_resume_back() {
 [ "$yoho_jt" = "$canon" ] || yoho_unlisted
 if [ -L "$canon" ]; then
  yoho_owned_link "$canon" && [ "$(readlink "$canon")" = "$yoho_jf" ] && yoho_marked "$yoho_jf" || yoho_unlisted
  yoho_candidate "$yoho_jf"
  yoho_idle "$yoho_jf"
  yoho_samedev "$yoho_jf" "$appparent"
  rm -f "$canon"
 fi
 if ! yoho_exists "$canon"; then
  yoho_marked "$yoho_jf" || yoho_unlisted
  yoho_candidate "$yoho_jf"
  yoho_idle "$yoho_jf"
  yoho_samedev "$yoho_jf" "$appparent"
  yoho_do_back "$yoho_jf"
 elif [ -d "$canon" ] && [ ! -L "$canon" ] && yoho_marked "$canon" && ! yoho_exists "$yoho_jf"; then
  rm -f "$journal"
 else
  yoho_unlisted
 fi
}
yoho_resume_retarget() {
 [ "$yoho_jf" != "$canon" ] && [ "$yoho_jt" != "$canon" ] || yoho_unlisted
 if yoho_marked "$yoho_jf" && yoho_vacant "$yoho_jt" && yoho_owned_link "$canon" && [ "$(readlink "$canon")" = "$yoho_jf" ]; then
  yoho_candidate "$yoho_jf"
  yoho_candidate "$yoho_jt"
  yoho_nested "$yoho_jf" "$yoho_jt"
  yoho_make "$(yoho_parent "$yoho_jt")" ancestor
  yoho_idle "$yoho_jf"
  yoho_samedev "$yoho_jf" "$(yoho_parent "$yoho_jt")"
  yoho_samedev "$meta" "$appparent"
  yoho_do_retarget "$yoho_jf" "$yoho_jt"
 elif yoho_marked "$yoho_jt" && ! yoho_exists "$yoho_jf" && yoho_owned_link "$canon" && [ "$(readlink "$canon")" = "$yoho_jf" ]; then
  yoho_candidate "$yoho_jt"
  yoho_samedev "$meta" "$appparent"
  yoho_publish "$yoho_jt"
  rm -f "$journal"
 elif [ -L "$canon" ] && [ "$(readlink "$canon")" = "$yoho_jt" ] && (yoho_linkok "$canon") 2>/dev/null; then
  if yoho_exists "$ltmp"; then
   yoho_owned_link "$ltmp" || yoho_refuse "$ltmp is not a symlink of the deploy user" "inspect it and remove it on the Server"
   rm -f "$ltmp"
  fi
  rm -f "$journal"
 else
  yoho_unlisted
 fi
}
yoho_resume() {
 [ -f "$journal" ] && [ ! -L "$journal" ] && [ -n "$(find "$journal" -prune -user "$yoho_uid" -print)" ] || yoho_refuse "$journal is not a regular file of the deploy user" "inspect it and remove it on the Server"
 yoho_j=$(cat "$journal")
 yoho_jk=$(printf '%s\n' "$yoho_j" | sed -n 's/^{"kind":"\([a-z]*\)","from":"\([^"\\]*\)","to":"\([^"\\]*\)"}$/\1/p')
 yoho_jf=$(printf '%s\n' "$yoho_j" | sed -n 's/^{"kind":"\([a-z]*\)","from":"\([^"\\]*\)","to":"\([^"\\]*\)"}$/\2/p')
 yoho_jt=$(printf '%s\n' "$yoho_j" | sed -n 's/^{"kind":"\([a-z]*\)","from":"\([^"\\]*\)","to":"\([^"\\]*\)"}$/\3/p')
 yoho_safe "$yoho_jf" && yoho_safe "$yoho_jt" || yoho_unlisted
 yoho_app_gate
 case "$yoho_jk" in
  forward) yoho_resume_forward;;
  back) yoho_resume_back;;
  retarget) yoho_resume_retarget;;
  *) yoho_unlisted;;
 esac
 yoho_resumed=$yoho_jk
}
yoho_fresh() {
 yoho_candidate "$want"
 yoho_samedev "$meta" "$appparent"
 if yoho_exists "$want"; then
  if yoho_marked "$want"; then
   yoho_private_dir "$want"
  elif yoho_empty "$want"; then
   yoho_private_dir "$want"
   yoho_mark "$want"
  elif yoho_exists "$want/.yoho-app.json"; then
   yoho_refuse "$want holds an ownership marker (.yoho-app.json) for another App or Destination" "choose another app_dir"
  else
   yoho_refuse "$want exists and is not empty" "app_dir must not exist yet or be an empty directory; Yoho owns it completely"
  fi
 else
  yoho_make "$want"
  yoho_mark "$want"
 fi
 yoho_publish "$want"
 yoho_action=created
}
yoho_target_vacant() {
 if yoho_vacant "$1"; then return 0; fi
 if yoho_marked "$1"; then
  yoho_refuse "both the current App directory and $1 hold this App's files" "keep one copy: move or remove the other by hand, then rerun"
 fi
 yoho_refuse "$1 exists and is not empty" "app_dir must not exist yet or be an empty directory; Yoho owns it completely"
}
yoho_forward() {
 yoho_private_dir "$canon"
 yoho_candidate "$want"
 yoho_target_vacant "$want"
 yoho_wp=$(yoho_parent "$want")
 yoho_make "$yoho_wp" ancestor
 yoho_idle "$canon"
 yoho_samedev "$canon" "$yoho_wp"
 yoho_samedev "$meta" "$appparent"
 yoho_app_gate
 yoho_journal forward "$canon" "$want"
 yoho_do_forward "$want"
 yoho_action=forward
}
yoho_back() {
 yoho_private_dir "$canon"
 yoho_candidate "$1"
 yoho_idle "$1"
 yoho_samedev "$1" "$appparent"
 yoho_app_gate
 yoho_journal back "$1" "$canon"
 rm -f "$canon"
 yoho_do_back "$1"
 yoho_action=back
}
yoho_retarget() {
 yoho_private_dir "$canon"
 yoho_candidate "$1"
 yoho_candidate "$2"
 yoho_nested "$1" "$2"
 yoho_target_vacant "$2"
 yoho_tp=$(yoho_parent "$2")
 yoho_make "$yoho_tp" ancestor
 yoho_idle "$1"
 yoho_samedev "$1" "$yoho_tp"
 yoho_samedev "$meta" "$appparent"
 yoho_app_gate
 yoho_journal retarget "$1" "$2"
 yoho_do_retarget "$1" "$2"
 yoho_action=retarget
}
`

const appDirMain = `mkdir -p "$meta"
yoho_gate_take "$gate" || { printf '%s\t%s\n' "$heldline" "$gate"; cat "$gate/info.json" 2>/dev/null || true; exit 0; }
yoho_resumed=
if yoho_exists "$journal"; then yoho_resume; fi
mkdir -p "$appparent"
yoho_action=used
if [ -L "$canon" ]; then
 (yoho_linkok "$canon") || yoho_refuse "$canon is a symlink Yoho does not trust" "Yoho trusts it only when the deploy user owns it and it points at a trusted directory holding this App's .yoho-app.json; inspect it and remove it only if it is not yours"
 yoho_cur=$(readlink "$canon")
 if [ -z "$want" ]; then
  yoho_back "$yoho_cur"
 elif [ "$yoho_cur" != "$want" ]; then
  yoho_retarget "$yoho_cur" "$want"
 else
  yoho_candidate "$yoho_cur"
  yoho_private_dir "$canon"
 fi
elif [ -d "$canon" ]; then
 # Validate before marking a directory that is about to move. Without
 # app_dir nothing moves, so existing layouts keep working as before.
 if [ -n "$want" ]; then yoho_private_dir "$canon"; fi
 yoho_mark "$canon"
 if [ -n "$want" ]; then yoho_forward; fi
elif yoho_exists "$canon"; then
 yoho_refuse "$canon is not a directory or symlink" "inspect it on the Server"
elif [ -z "$want" ]; then
 mkdir "$canon"
 yoho_mark "$canon"
 yoho_action=created
else
 yoho_fresh
fi
printf 'ok\t%s\t%s\t%s\n' "$yoho_action" "$yoho_resumed" "$(cd -P "$canon" && pwd -P)"
`

// AppDirState is the read-only view of a canonical App directory for plan.
type AppDirState struct {
	// Kind: missing, dir, link or other.
	Kind string
	// Target is the link target when Kind is link.
	Target string
	// Journal is the migration journal content, when one exists.
	Journal string
}

// ReadAppDir inspects the canonical App directory without changing
// anything (plan, status).
func ReadAppDir(ctx context.Context, h remote.Host, app, dest string) (AppDirState, error) {
	c, j := remote.Quote(release.AppDir(app, dest)), remote.Quote(safepath.JournalPath(app, dest))
	script := "if [ -e " + j + " ] || [ -L " + j + " ]; then printf 'journal\\t'; tr '\\n' ' ' < " + j + " 2>/dev/null || true; echo; fi\n" +
		"if [ -L " + c + " ]; then printf 'link\\t'; readlink " + c + "\n" +
		"elif [ -d " + c + " ]; then echo dir\nelif [ -e " + c + " ]; then echo other\nelse echo missing; fi\n"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return AppDirState{}, fmt.Errorf("read App directory on %s: %w", h.Name(), err)
	}
	var s AppDirState
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "\t")
		switch k {
		case "journal":
			s.Journal = strings.TrimSpace(v)
			if s.Journal == "" {
				s.Journal = "(empty)"
			}
		case "link":
			s.Kind, s.Target = k, strings.TrimSpace(v)
		case "dir", "other", "missing":
			s.Kind = k
		}
	}
	if s.Kind == "" {
		return s, fmt.Errorf("read App directory on %s: unexpected output %q", h.Name(), out)
	}
	return s, nil
}

// PlanAppDir returns the planned change of the canonical App directory for
// want ("" is the default layout), or ok=false when there is none.
func PlanAppDir(s AppDirState, app, dest, want string) (plan.Change, bool) {
	canon := release.AppDir(app, dest)
	ch := plan.Change{Kind: "app_dir", Name: canon, Action: plan.ActionUpdate}
	show := func(p string) string {
		if p == "" || p == canon {
			return canon + " (default)"
		}
		return p
	}
	switch {
	case s.Journal != "":
		ch.Reasons = []string{"finish interrupted app_dir migration " + s.Journal}
		if want != "" || s.Kind == "link" {
			ch.Reasons = append(ch.Reasons, "app_dir: "+show(s.Target)+" → "+show(want))
		}
	case s.Kind == "missing" && want == "":
		return ch, false
	case s.Kind == "missing":
		ch.Action = plan.ActionCreate
		ch.Reasons = []string{"app_dir: " + want}
	case s.Kind == "dir" && want == "":
		return ch, false
	case s.Kind == "dir":
		ch.Reasons = []string{"app_dir: " + show("") + " → " + want, "moves the App directory (same filesystem rename)"}
	case s.Kind == "link" && s.Target == want:
		return ch, false
	case s.Kind == "link":
		ch.Reasons = []string{"app_dir: " + show(s.Target) + " → " + show(want), "moves the App directory (same filesystem rename)"}
	default:
		ch.Reasons = []string{canon + " is not a directory or symlink; apply will refuse"}
	}
	return ch, true
}
