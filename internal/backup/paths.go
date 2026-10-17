package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"

	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// pathSafety and pathCreation are the shared Server path trust functions
// (internal/safepath), labelled for Backup refusals. Behaviour is unchanged
// unless a script also sets yoho_applink (see appLink).
var (
	pathSafety   = safepath.Script("Backup")
	pathCreation = safepath.Creation
)

// appLink enables the one trusted link: the canonical App directory of
// app/dest when it is a Yoho-owned symlink to a marked app_dir.
func appLink(app, dest string) string { return safepath.AppLink(app, dest) }

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
