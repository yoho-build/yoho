package backup

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/yoho-build/yoho/internal/remote"
)

func publishArchive(ctx context.Context, h remote.Host, source, repo, name string) error {
	tempName, token, err := newStagingName(".yoho-publish")
	if err != nil {
		return err
	}
	return publishArchiveAt(ctx, h, source, repo, name, tempName, token)
}

// Cleanup lives outside the remote shell: even SIGKILL of that shell, a lost
// reply, or cancellation must remove our proven temporary link. Otherwise the
// committed Backup would stay multiply linked and correctly fail read checks.
// A killed Yoho process cannot run this defer; its marked temp needs recovery.
func publishArchiveAt(ctx context.Context, h remote.Host, source, repo, name, tempName, token string) (err error) {
	tmp := path.Join(repo, tempName)
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := removePrivateStaging(cctx, h, tmp, token); cerr != nil {
			err = errors.Join(err, fmt.Errorf("remove private archive publication staging %s: %w", tmp, cerr))
		}
	}()
	if err := createStaging(ctx, h, repo, tmp, token, false); err != nil {
		return fmt.Errorf("create archive publication staging %s: %w", tmp, err)
	}
	return h.Run(ctx, remote.Cmd{Script: publishArchiveScript(source, repo, name, tmp)})
}

// ln without -f never overwrites an existing file; -n refuses a destination
// symlink to a directory (GNU/BSD/BusyBox). Concurrent publication of one ID
// has one complete winner. The temp and marker were exclusively created above.
func publishArchiveScript(source, repo, name, tmp string) string {
	archive := path.Join(tmp, "archive")
	return safeDir(tmp, true, false) + safeFile(source, false) +
		"test ! -e " + remote.Quote(archive) + " && test ! -L " + remote.Quote(archive) + "\n" +
		"cp " + remote.Quote(source) + " " + remote.Quote(archive) + "\n" +
		"chmod 0600 " + remote.Quote(archive) + "\n" +
		"yoho_file " + remote.Quote(archive) + "\n" +
		"if [ -e " + remote.Quote(path.Join(repo, name)) + " ] || [ -L " + remote.Quote(path.Join(repo, name)) + " ]; then yoho_bad " + remote.Quote(path.Join(repo, name)) + "; fi\n" +
		"ln -n " + remote.Quote(archive) + " " + remote.Quote(path.Join(repo, name)) + "\n"
}
