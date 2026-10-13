package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func privateTemp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	return d
}
func localScript(script string) error {
	return (&remote.Local{}).Run(context.Background(), remote.Cmd{Script: script})
}
func TestBackupPathTrust(t *testing.T) {
	for _, kind := range []string{"valid", "sticky-parent", "shared-parent", "linked-parent", "linked-leaf", "public-private-leaf", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTemp(t)
			parent := filepath.Join(root, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(parent, "private")
			if err := os.Mkdir(leaf, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "sticky-parent":
				os.Chmod(parent, os.ModeSticky|0777)
			case "shared-parent":
				os.Chmod(parent, 0777)
			case "linked-parent":
				os.Rename(parent, parent+"-real")
				os.Symlink(parent+"-real", parent)
			case "linked-leaf":
				os.Rename(leaf, leaf+"-real")
				os.Symlink(leaf+"-real", leaf)
			case "public-private-leaf":
				os.Chmod(leaf, 0755)
			}
			s := safeDir(leaf, true, false)
			if kind == "foreign-owner" {
				s = strings.Replace(s, "yoho_uid=$(id -u)", "yoho_uid=65534", 1)
			}
			err := localScript(s)
			valid := kind == "valid" || kind == "sticky-parent"
			// A root-owned ancestor link is an explicitly trusted admin alias.
			if kind == "linked-parent" && os.Getuid() == 0 {
				valid = true
			}
			if (err == nil) != valid {
				t.Fatalf("valid=%v err=%v", valid, err)
			}
			if err != nil && !strings.Contains(err.Error(), "unsafe Backup path") {
				t.Fatal(err)
			}
		})
	}
}

func TestArchivePublicationNonOverwriteAndCollisionCleanup(t *testing.T) {
	for _, kind := range []string{"file", "symlink-file", "symlink-dir", "parent-link", "unsafe-parent", "staging-collision", "valid"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTemp(t)
			source := filepath.Join(root, "source")
			os.WriteFile(source, []byte("new archive"), 0600)
			repo := filepath.Join(root, "repo")
			os.Mkdir(repo, 0700)
			outside := filepath.Join(root, "foreign")
			os.WriteFile(outside, []byte("untouched"), 0600)
			name := "backup.tar.gz"
			dst := filepath.Join(repo, name)
			switch kind {
			case "file":
				os.WriteFile(dst, []byte("old archive"), 0600)
			case "symlink-file":
				os.Symlink(outside, dst)
			case "symlink-dir":
				os.Symlink(root, dst)
			case "parent-link":
				os.Rename(repo, repo+"-real")
				os.Symlink(repo+"-real", repo)
			case "unsafe-parent":
				os.Chmod(repo, 0777)
			case "staging-collision":
				os.Mkdir(filepath.Join(repo, ".yoho-publish-collision"), 0700)
				os.WriteFile(filepath.Join(repo, ".yoho-publish-collision", "foreign"), []byte("untouched"), 0600)
			}
			err := publishArchiveAt(context.Background(), &remote.Local{}, source, repo, name, ".yoho-publish-collision", "owned-token")
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s err=%v", kind, err)
			}
			b, _ := os.ReadFile(outside)
			if string(b) != "untouched" {
				t.Fatal("foreign file changed")
			}
			if kind == "file" {
				b, _ = os.ReadFile(dst)
				if string(b) != "old archive" {
					t.Fatal("existing Backup overwritten")
				}
			}
			if kind == "staging-collision" {
				b, _ = os.ReadFile(filepath.Join(repo, ".yoho-publish-collision", "foreign"))
				if string(b) != "untouched" {
					t.Fatal("foreign temporary directory removed")
				}
			}
			if kind == "valid" {
				b, _ = os.ReadFile(dst)
				if string(b) != "new archive" {
					t.Fatalf("published %q", b)
				}
				fi, _ := os.Stat(dst)
				if fi.Mode().Perm() != 0600 {
					t.Fatal(fi.Mode())
				}
			}
		})
	}
}
func TestConcurrentArchivePublishOneCompleteWinner(t *testing.T) {
	root := privateTemp(t)
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	const name = "backup.tar.gz"
	payloads := []string{strings.Repeat("a", 128*1024), strings.Repeat("b", 128*1024)}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range payloads {
		source := filepath.Join(root, []string{"a", "b"}[i])
		os.WriteFile(source, []byte(payloads[i]), 0600)
		wg.Add(1)
		go func(i int, source string) {
			defer wg.Done()
			errs[i] = publishArchiveAt(context.Background(), &remote.Local{}, source, repo, name, ".yoho-publish-"+[]string{"a", "b"}[i], "owned-token-"+[]string{"a", "b"}[i])
		}(i, source)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want one winner: %v", errs)
	}
	b, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil || (string(b) != payloads[0] && string(b) != payloads[1]) {
		t.Fatal("publication incomplete", err)
	}
	left, _ := filepath.Glob(filepath.Join(repo, ".yoho-publish-*"))
	if len(left) != 0 {
		t.Fatal("temporary publication dirs left", left)
	}
}

func TestGeneratedSecretsRejectLinkedSourceAndDestination(t *testing.T) {
	old := release.Root
	release.Root = privateTemp(t)
	t.Cleanup(func() { release.Root = old })
	h := &remote.Local{}
	ctx := context.Background()
	dir := generatedDir("shop", "production")
	os.MkdirAll(dir, 0700)
	outside := filepath.Join(release.Root, "outside")
	os.WriteFile(outside, []byte("secret-should-not-appear"), 0600)
	src := filepath.Join(dir, "PG")
	os.Symlink(outside, src)
	if _, err := ListGenerated(ctx, h, "shop", "production"); err == nil || strings.Contains(err.Error(), "secret-should-not-appear") {
		t.Fatalf("linked list: %v", err)
	}
	staging := filepath.Join(release.Root, "staging")
	os.Mkdir(staging, 0700)
	if err := stageGenerated(ctx, h, "shop", "production", staging, []string{"PG"}); err == nil {
		t.Fatal("linked source staged")
	}
	data := filepath.Join(release.Root, "data")
	os.MkdirAll(filepath.Join(data, generatedPrefix), 0700)
	os.WriteFile(filepath.Join(data, generatedPrefix, "PG"), []byte("restored"), 0600)
	if _, _, err := restoreGenerated(ctx, h, "shop", "production", data, []string{"PG"}, func(string, ...any) {}); err == nil {
		t.Fatal("linked destination accepted")
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "secret-should-not-appear" {
		t.Fatal("foreign secret changed")
	}
	os.Remove(src)
	os.WriteFile(src, []byte("old"), 0644)
	if _, _, err := restoreGenerated(ctx, h, "shop", "production", data, []string{"PG"}, func(string, ...any) {}); err == nil {
		t.Fatal("public generated secret accepted")
	}
}

func TestFetchAndPruneRejectArchiveSymlink(t *testing.T) {
	root := privateTemp(t)
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	outside := filepath.Join(root, "foreign")
	os.WriteFile(outside, []byte("untouched"), 0600)
	tget, err := newTarget("shop", "production", release.ProjectName("shop", "production"), config.BackupTarget{Type: "archive", Repository: repo, KeepLast: 1}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := "yoho-shop-production-20261001T030000Z.tar.gz"
	os.Symlink(outside, filepath.Join(repo, id))
	os.WriteFile(filepath.Join(repo, "yoho-shop-production-20261002T030000Z.tar.gz"), []byte("new"), 0600)
	staging := filepath.Join(root, "stage")
	os.Mkdir(staging, 0700)
	h := &remote.Local{}
	if _, err := tget.fetchArchive(context.Background(), h, staging, id, false); err == nil {
		t.Fatal("linked archive fetched")
	}
	if err := tget.pruneArchives(context.Background(), h); err == nil {
		t.Fatal("linked archive pruned")
	}
	if _, err := os.Lstat(filepath.Join(repo, id)); err != nil {
		t.Fatal("foreign link deleted", err)
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "untouched" {
		t.Fatal("foreign archive changed")
	}
}

func TestOuterArchiveExtractorOnlyWritableDataMount(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip", "7z"} {
		h := &fakeHost{}
		tg, _ := newTarget("shop", "production", release.ProjectName("shop", "production"), config.BackupTarget{Type: "archive", Format: format, Repository: "remote:backups"}, "", nil)
		_, err := tg.fetchArchive(context.Background(), h, "/owned/stage", "yoho-shop-production-20261001T030000Z."+format, false)
		if err != nil {
			t.Fatal(err)
		}
		var extract string
		for _, c := range h.cmds {
			if strings.Contains(c.Script, "--cap-drop ALL") {
				extract = c.Script
			}
		}
		for _, want := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", `--user "$(id -u):$(id -g)"`, "/archive:ro", "/data:/out", "-type l", "-links +1"} {
			if !strings.Contains(extract, want) {
				t.Fatalf("missing %s: %s", want, extract)
			}
		}
		if len(checkMountRe.FindAllStringSubmatch(extract, -1)) != 2 || strings.Contains(extract, "stage:/s") || strings.Contains(extract, "chown -R") {
			t.Fatal("extractor exposes parent or changes link targets", extract)
		}
	}
}

// These payloads contain only inert archive entries, never executable source.
// Local helpers substitute temp paths; sandbox mount confinement is asserted
// separately, not claimed to have been exercised without a Docker daemon.
func TestOuterArchiveRejectsSymlinkAndHardlinkEntries(t *testing.T) {
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink} {
		t.Run(string(kind), func(t *testing.T) {
			root := privateTemp(t)
			stage := filepath.Join(root, "stage")
			os.Mkdir(stage, 0700)
			repo := filepath.Join(root, "repo")
			os.Mkdir(repo, 0700)
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			tw.WriteHeader(&tar.Header{Name: "payload", Mode: 0600, Size: 1})
			tw.Write([]byte("x"))
			tw.WriteHeader(&tar.Header{Name: "link", Mode: 0600, Typeflag: kind, Linkname: "payload"})
			tw.Close()
			gz.Close()
			id := "yoho-shop-production-20261001T030000Z.tar.gz"
			os.WriteFile(filepath.Join(repo, id), b.Bytes(), 0600)
			tg, _ := newTarget("shop", "production", release.ProjectName("shop", "production"), config.BackupTarget{Type: "archive", Repository: repo}, "", nil)
			if _, err := tg.fetchArchive(context.Background(), &checkLocalHost{}, stage, id, false); err == nil {
				t.Fatal("linked outer entry accepted")
			}
		})
	}
}

func TestCurrentReleaseAllowsOnlyOwnedReleaseTree(t *testing.T) {
	root := privateTemp(t)
	app := filepath.Join(root, "apps", "shop", "prod")
	ver := filepath.Join(app, "releases", "v1")
	os.MkdirAll(ver, 0700)
	os.WriteFile(filepath.Join(ver, "plan.json"), []byte("[]"), 0600)
	os.Symlink("releases/v1", filepath.Join(app, "current"))
	if err := localScript(safeCurrentFile(app, "plan.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "foreign")
	os.Mkdir(outside, 0700)
	os.WriteFile(filepath.Join(outside, "plan.json"), []byte("[]"), 0600)
	os.Remove(filepath.Join(app, "current"))
	os.Symlink(outside, filepath.Join(app, "current"))
	if err := localScript(safeCurrentFile(app, "plan.json")); err == nil {
		t.Fatal("foreign current Release accepted")
	}
}

func TestPrivateStagingCleanupRejectsRedirectedParent(t *testing.T) {
	root := privateTemp(t)
	base := filepath.Join(root, "spool")
	dir := filepath.Join(base, "stage")
	h := &remote.Local{}
	ctx := context.Background()
	if err := createPrivateStaging(ctx, h, base, dir, "token"); err != nil {
		t.Fatal(err)
	}
	actual := base + "-actual"
	if err := os.Rename(base, actual); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, base); err != nil {
		t.Fatal(err)
	}
	if err := removePrivateStaging(ctx, h, dir, "token"); err == nil {
		t.Fatal("redirected cleanup accepted")
	}
	if _, err := os.Stat(filepath.Join(actual, "stage", checkOwnerMarker)); err != nil {
		t.Fatal("owned original deleted through symlink", err)
	}
}

func TestLocalTargetPathValidationAndDefaults(t *testing.T) {
	for _, repo := range []string{"relative", "local:relative", "/safe/../foreign", "/safe:foreign"} {
		if _, err := newTarget("app", "prod", "yoho-app-prod", config.BackupTarget{Repository: repo}, "password", nil); err == nil {
			t.Fatalf("accepted %q", repo)
		}
	}
	tg, err := newTarget("app", "prod", "yoho-app-prod", config.BackupTarget{Repository: "local:/safe/repo"}, "password", nil)
	if err != nil || tg.typ != "restic" || tg.cfg.Repository != "/safe/repo" {
		t.Fatalf("local restic: %+v %v", tg, err)
	}
	tg, err = newTarget("app", "prod", "yoho-app-prod", config.BackupTarget{Type: "archive", Repository: "/safe/repo"}, "", nil)
	if err != nil || tg.format != "tar.gz" {
		t.Fatalf("default archive: %+v %v", tg, err)
	}
}

func TestMissingPathCreationDoesNotFollowInsertedLink(t *testing.T) {
	root := privateTemp(t)
	outside := filepath.Join(root, "foreign")
	os.Mkdir(outside, 0700)
	component := filepath.Join(root, "new")
	leaf := filepath.Join(component, "child")
	// Trusted test shim simulates a competing name claim exactly at mkdir.
	shim := "mkdir() { if [ \"$3\" = " + remote.Quote(component) + " ]; then command ln -s " + remote.Quote(outside) + " " + remote.Quote(component) + "; fi; command mkdir \"$@\"; }\n"
	script := pathSafety + pathCreation + shim + "yoho_make " + remote.Quote(leaf) + "\n"
	if err := localScript(script); err == nil {
		t.Fatal("inserted link reused")
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !os.IsNotExist(err) {
		t.Fatal("foreign target was written", err)
	}
}

type publicationInterruptHost struct {
	remote.Local
	kind, dir       string
	cancel          context.CancelFunc
	cleanupDetached bool
}

func (h *publicationInterruptHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "rm -rf") {
		h.cleanupDetached = ctx.Err() == nil
		if h.kind == "cleanup-failure" {
			return errors.New("cleanup unavailable")
		}
	}
	if m := stagingDirRe.FindStringSubmatch(c.Script); m != nil && strings.Contains(m[1], ".yoho-publish-") {
		h.dir = m[1]
		if h.kind == "collision" {
			os.Mkdir(h.dir, 0700)
			os.WriteFile(filepath.Join(h.dir, "foreign"), []byte("untouched"), 0600)
		}
		if h.kind == "unproven" {
			c.Script = strings.Split(c.Script, "\n(set -C;")[0]
		}
		if err := h.Local.Run(ctx, c); err != nil {
			return err
		}
		if h.kind == "creation-lost-ack" || h.kind == "unproven" {
			return errors.New("creation reply lost")
		}
		return nil
	}
	if i := strings.Index(c.Script, "\nln -n "); i >= 0 {
		switch h.kind {
		case "precommit":
			c.Script = c.Script[:i]
		case "kill-after-link":
			c.Script += "\nkill -KILL $$\n"
		}
		if err := h.Local.Run(ctx, c); err != nil {
			return err
		}
		if h.kind == "cancel-after-link" {
			h.cancel()
			return context.Canceled
		}
		if h.kind == "lost-ack" || h.kind == "precommit" {
			return errors.New("publication reply lost")
		}
		return nil
	}
	return h.Local.Run(ctx, c)
}
func TestInterruptedArchivePublicationCleansOnlyProvenTemporaryLink(t *testing.T) {
	for _, kind := range []string{"lost-ack", "kill-after-link", "cancel-after-link", "precommit", "creation-lost-ack", "collision", "unproven", "cleanup-failure"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTemp(t)
			repo := filepath.Join(root, "repo")
			os.Mkdir(repo, 0755)
			source := filepath.Join(root, "source")
			os.WriteFile(source, []byte("complete archive"), 0600)
			const name = "yoho-shop-production-20261001T030000Z.tar.gz"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &publicationInterruptHost{kind: kind, cancel: cancel}
			err := publishArchive(ctx, h, source, repo, name)
			if err == nil || !h.cleanupDetached || h.dir == "" {
				t.Fatalf("err=%v detached=%v dir=%s", err, h.cleanupDetached, h.dir)
			}
			committed := kind == "lost-ack" || kind == "kill-after-link" || kind == "cancel-after-link" || kind == "cleanup-failure"
			dst := filepath.Join(repo, name)
			b, readErr := os.ReadFile(dst)
			if committed {
				if readErr != nil || string(b) != "complete archive" {
					t.Fatalf("committed file changed: %q %v", b, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("unexpected commit %v", readErr)
			}
			keep := kind == "collision" || kind == "unproven" || kind == "cleanup-failure"
			_, statErr := os.Lstat(h.dir)
			if keep {
				if statErr != nil || !strings.Contains(err.Error(), h.dir) {
					t.Fatalf("remaining path not reported: %v %v", err, statErr)
				}
				if kind == "collision" {
					b, _ := os.ReadFile(filepath.Join(h.dir, "foreign"))
					if string(b) != "untouched" {
						t.Fatal("foreign collision deleted")
					}
				}
			} else {
				if !os.IsNotExist(statErr) {
					t.Fatalf("proven publication temp remains: %v", statErr)
				}
				if committed {
					if err := localScript(safeFile(dst, false)); err != nil {
						t.Fatal("committed archive remains multiply linked", err)
					}
					tg, _ := newTarget("shop", "production", release.ProjectName("shop", "production"), config.BackupTarget{Type: "archive", Repository: repo}, "", nil)
					entries, err := tg.list(context.Background(), &remote.Local{}, "")
					if err != nil || len(entries) != 1 || entries[0].ID != name {
						t.Fatalf("committed archive not readable/listable: %v %v", entries, err)
					}
				}
			}
		})
	}
}
func TestRestoreFinalStagingCleanupFailureFailsClosed(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			r := newRestoreRig(t, runtime, false)
			respond := r.h.respond
			r.h.respond = func(ctx context.Context, script string) (string, error) {
				if strings.Contains(script, "rm -rf '/var/lib/yoho/backups/shop/production/restore-") {
					return "", errors.New("staging cleanup unavailable")
				}
				return respond(ctx, script)
			}
			_, err := Restore(context.Background(), r.o)
			if err == nil || !strings.Contains(err.Error(), "Services left stopped") || !strings.Contains(err.Error(), "staged files may be incomplete") {
				t.Fatalf("cleanup failure: %v", err)
			}
			assertRestoreStopped(t, r)
		})
	}
}
