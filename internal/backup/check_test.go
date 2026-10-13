package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func checkOptions(h remote.Host) CheckOptions {
	return CheckOptions{ListOptions: ListOptions{App: "shop", Destination: "production", Host: h, Target: config.BackupTarget{Repository: "s3:repo", PasswordSecret: "PW"}, Password: "test-secret"}}
}

func TestCheckResticReadOnly(t *testing.T) {
	for _, data := range []bool{false, true} {
		h := &fakeHost{respond: func(_ context.Context, s string) (string, error) {
			if strings.Contains(s, "r snapshots") {
				return `[{"short_id":"abcdef12","tags":["yoho","app=shop","destination=production"]}]`, nil
			}
			return "", nil
		}}
		o := checkOptions(h)
		o.ReadData = data
		locked, unlocked := 0, 0
		o.Lock = func(context.Context) (func(context.Context) error, error) {
			locked++
			return func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				unlocked++
				return nil
			}, nil
		}
		res, err := Check(context.Background(), o)
		if err != nil || res.Method != "restic" || locked != 1 || unlocked != 1 {
			t.Fatalf("%+v %v lock %d/%d", res, err, locked, unlocked)
		}
		sc := h.scripts()
		for _, bad := range []string{"r init", "r restore", "r forget", "mkdir", ".restic-cache", "test-secret", "docker volume", "docker compose"} {
			if strings.Contains(sc, bad) {
				t.Errorf("unsafe check command %s: %s", bad, sc)
			}
		}
		if !strings.Contains(sc, "--no-lock --no-cache") || !strings.Contains(sc, "r check") || strings.Contains(sc, "r check --read-data") != data {
			t.Fatal(sc)
		}
	}
}

func TestCheckResticEmptyMissingCorruptAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		fail      string
	}{
		{"empty", "[]", ""}, {"foreign", "[]", ""}, {"missing", "", "r snapshots"}, {"corrupt", `[{"short_id":"abcdef12","tags":["yoho","app=shop","destination=production"]}]`, "r check"}, {"bad-json", "bad", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHost{respond: func(_ context.Context, s string) (string, error) {
				if tc.fail != "" && strings.Contains(s, tc.fail) {
					return "", errors.New("test-secret failed")
				}
				if strings.Contains(s, "r snapshots") {
					return tc.out, nil
				}
				return "", nil
			}}
			_, err := Check(context.Background(), checkOptions(h))
			if err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("%v", err)
			}
			if strings.Contains(h.scripts(), "r init") {
				t.Fatal("initialized repository")
			}
		})
	}
}

func TestCheckRejectsBadOptionsBeforeHost(t *testing.T) {
	for _, tc := range []struct {
		typ, id, app string
		read         bool
	}{{"restic", "latest", "shop", false}, {"archive", "", "shop", true}, {"archive", "../../x", "shop", false}, {"restic", "", "../x", false}} {
		h := &fakeHost{}
		o := checkOptions(h)
		o.Target.Type = tc.typ
		o.Target.Repository = "/repo"
		o.Password = ""
		if tc.typ == "restic" {
			o.Password = "test-secret"
		}
		o.ID, o.App, o.ReadData = tc.id, tc.app, tc.read
		if _, err := Check(context.Background(), o); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
		if len(h.cmds) != 0 {
			t.Fatal(h.scripts())
		}
	}
}

// checkLocalHost executes only the generated tar/helper shell in temporary
// directories, substituting local tar for Docker. No daemon/images/Server.
type checkLocalHost struct {
	remote.Local
	cmds []remote.Cmd
}

var checkMountRe = regexp.MustCompile(`-v '([^']+):/(s|in)(?::ro)?'`)

func (h *checkLocalHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}
func (h *checkLocalHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.cmds = append(h.cmds, c)
	if strings.Contains(c.Script, "docker run") {
		m := checkMountRe.FindStringSubmatch(c.Script)
		if m == nil {
			return "", fmt.Errorf("unexpected helper: %s", c.Script)
		}
		if i := strings.Index(c.Script, " -c '"); i >= 0 {
			tail := c.Script[i+5:]
			end := strings.LastIndex(tail, "' sh ")
			args := ""
			if end >= 0 {
				args = tail[end+1:]
				tail = tail[:end]
			} else {
				tail = strings.TrimSuffix(tail, "'")
			}
			inner := strings.ReplaceAll(tail, "'\\''", "'")
			inner = strings.ReplaceAll(inner, "/"+m[2], m[1])
			c.Script = "set -eu\nuid=$(id -u); gid=$(id -g)\nsh -c " + remote.Quote(inner) + args
		} else if i := strings.Index(c.Script, " tar -tzf "); i >= 0 {
			c.Script = "set -eu\ntar -tzf " + strings.ReplaceAll(c.Script[i+len(" tar -tzf "):], "/s", m[1])
		} else {
			return "", fmt.Errorf("unexpected Docker helper: %s", c.Script)
		}
	}
	return h.Local.Output(ctx, c)
}
func tarBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func validCheckManifest() Manifest {
	return Manifest{App: "shop", Destination: "production", Project: release.ProjectName("shop", "production"), CreatedAt: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), Services: map[string]ManifestService{"db": {Volumes: map[string]string{"data": "db-data.tar.gz"}, Dump: "db.dump", Quiesce: "pause"}}}
}
func TestArchiveCheckLocalIntegrityAndCleanup(t *testing.T) {
	oldRoot := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = oldRoot })
	for _, kind := range []string{"valid", "missing-manifest", "foreign-manifest", "missing-volume", "corrupt-volume", "missing-dump", "bad-name", "empty-manifest", "corrupt-outer", "empty-repo"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			h := &checkLocalHost{}
			o := checkOptions(h)
			o.Target = config.BackupTarget{Type: "archive", Repository: repo}
			o.Password = ""
			m := validCheckManifest()
			if kind == "foreign-manifest" {
				m.App = "foreign"
			}
			if kind == "bad-name" {
				ms := m.Services["db"]
				ms.Dump = "../outside"
				m.Services["db"] = ms
			}
			if kind == "empty-manifest" {
				m.Services = nil
			}
			mb, _ := json.Marshal(m)
			files := map[string][]byte{"manifest.json": mb, "db-data.tar.gz": tarBytes(t, map[string][]byte{"data": []byte("payload")}), "db.dump": []byte("SQL dump")}
			if kind == "missing-manifest" {
				delete(files, "manifest.json")
			}
			if kind == "missing-volume" {
				delete(files, "db-data.tar.gz")
			}
			if kind == "missing-dump" {
				delete(files, "db.dump")
			}
			if kind == "corrupt-volume" {
				files["db-data.tar.gz"] = []byte("not gzip")
			}
			id := oAppProject(o) + "-20261013T000000Z.tar.gz"
			archive := tarBytes(t, files)
			if kind == "corrupt-outer" {
				archive = archive[:len(archive)/2]
			}
			if kind != "empty-repo" {
				if err := os.WriteFile(filepath.Join(repo, id), archive, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// An unrelated Backup is never selected or changed.
			foreign := filepath.Join(repo, "yoho-other-prod-20261013T000000Z.tar.gz")
			os.WriteFile(foreign, []byte("untouched"), 0600)
			res, err := Check(context.Background(), o)
			if kind == "valid" {
				if err != nil || res.ID != id {
					t.Fatalf("%+v %v", res, err)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", kind)
			}
			if b, _ := os.ReadFile(foreign); string(b) != "untouched" {
				t.Fatal("foreign Backup changed")
			}
			dirs, _ := filepath.Glob(filepath.Join(release.BackupDir(o.App, o.Destination), "check-*"))
			if len(dirs) != 0 {
				t.Fatalf("staging left %v", dirs)
			}
			for _, c := range h.cmds {
				if strings.Contains(c.Script, "docker volume") || strings.Contains(c.Script, "docker compose") || strings.Contains(c.Script, "r restore") {
					t.Fatal(c.Script)
				}
			}
		})
	}
}
func oAppProject(o CheckOptions) string { return release.ProjectName(o.App, o.Destination) }

func TestCheckCancellationCleansPrivateStagingAndUnlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &fakeHost{}
	created := false
	cleaned := false
	h.respond = func(cctx context.Context, s string) (string, error) {
		if strings.Contains(s, "mkdir -m 0700") {
			created = true
			return "", nil
		}
		if strings.Contains(s, "cp ") {
			cancel()
			return "", context.Canceled
		}
		if strings.Contains(s, "rm -rf") {
			if cctx.Err() != nil {
				t.Fatal("cleanup cancelled")
			}
			cleaned = true
		}
		return "", nil
	}
	o := checkOptions(h)
	o.Target = config.BackupTarget{Type: "archive", Repository: "/repo"}
	o.Password = ""
	o.ID = "yoho-shop-production-20261013T000000Z.tar.gz"
	unlocked := false
	o.Lock = func(context.Context) (func(context.Context) error, error) {
		return func(ctx context.Context) error { unlocked = ctx.Err() == nil; return nil }, nil
	}
	_, err := Check(ctx, o)
	if !errors.Is(err, context.Canceled) || !created || !cleaned || !unlocked {
		t.Fatalf("%v created=%v cleaned=%v unlocked=%v", err, created, cleaned, unlocked)
	}
}

func TestCheckEncryptedArchivesUsePrivateReadOnlyValidation(t *testing.T) {
	for _, format := range []string{"zip", "7z"} {
		for _, corrupt := range []bool{false, true} {
			h := &fakeHost{files: map[string][]byte{}}
			h.respond = func(ctx context.Context, sc string) (string, error) {
				if strings.Contains(sc, " x -bd ") {
					m := checkMountRe.FindStringSubmatch(sc)
					if m == nil {
						t.Fatal(sc)
					}
					manifest, _ := json.Marshal(validCheckManifest())
					h.files[filepath.Join(m[1], "data", "manifest.json")] = manifest
				}
				if corrupt && strings.Contains(sc, " t -bd") {
					return "", errors.New("CRC error " + secretPW)
				}
				return "", nil
			}
			o := checkOptions(h)
			o.Target = config.BackupTarget{Type: "archive", Format: format, Repository: "remote:backups", PasswordSecret: "PW"}
			o.Password = secretPW
			o.ID = "yoho-shop-production-20261013T000000Z." + format
			_, err := Check(context.Background(), o)
			if (err != nil) != corrupt {
				t.Fatalf("format=%s corrupt=%v err=%v", format, corrupt, err)
			}
			if err != nil && strings.Contains(err.Error(), secretPW) {
				t.Fatal("secret in error")
			}
			if sc := h.scripts(); strings.Contains(sc, secretPW) || !strings.Contains(sc, `--user "$uid:$gid"`) || !strings.Contains(sc, " t -bd") || !strings.Contains(sc, ":/s:ro") {
				t.Fatal(sc)
			}
			if h.index("rm -rf") < 0 {
				t.Fatal("staging not removed")
			}
		}
	}
}

func TestBackupRetrySafetyMarkersAndKnownStoredID(t *testing.T) {
	for _, kind := range []string{"copy", "unpause", "storage", "retention"} {
		h := &fakeHost{respond: dockerResponder(func(sc string) (string, error) {
			switch {
			case kind == "copy" && strings.Contains(sc, "tar -C /data -czf"):
				return "", errors.New("copy failed")
			case kind == "unpause" && strings.Contains(sc, "docker unpause"):
				return "", errors.New("unpause uncertain")
			case strings.Contains(sc, "r backup --json"):
				if kind == "storage" {
					return "", errors.New("reply lost after storage")
				}
				return `{"message_type":"summary","snapshot_id":"abcdef123456","total_bytes_processed":12}`, nil
			case kind == "retention" && strings.Contains(sc, "r forget"):
				return "", errors.New("retention failed")
			}
			return "", nil
		})}
		o := baseOpts(h)
		o.Services = map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}}}
		o.Target = config.BackupTarget{Repository: "s3:repo", KeepLast: 1}
		o.Password = secretPW
		res, err := Run(context.Background(), o)
		if err == nil || UnsafeToRetry(err) != (kind != "copy") {
			t.Fatalf("kind %s res %+v err %v unsafe=%v", kind, res, err, UnsafeToRetry(err))
		}
		if kind == "retention" && (res == nil || res.ID != "abcdef12") {
			t.Fatalf("lost stored ID: %+v %v", res, err)
		}
	}
}

// stagingFailureHost executes only staging shell in temporary directories and
// simulates replies lost after remote creation or marker-write failure.
type stagingFailureHost struct {
	remote.Local
	kind            string
	dir             string
	cleanupDetached bool
	cancel          context.CancelFunc
}

var stagingDirRe = regexp.MustCompile(`mkdir -m 0700 '([^']+)'`)

func (h *stagingFailureHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "mkdir -m 0700") {
		match := stagingDirRe.FindStringSubmatch(c.Script)
		if match == nil {
			return errors.New("missing staging path")
		}
		h.dir = match[1]
		if h.kind == "collision" {
			if err := os.MkdirAll(h.dir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(h.dir, "foreign"), []byte("untouched"), 0600); err != nil {
				return err
			}
			return h.Local.Run(ctx, c) // mkdir EEXIST must not write our marker
		}
		if h.kind == "marker-failure" {
			c.Script = strings.Split(c.Script, "\nprintf '%s' ")[0]
		}
		if err := h.Local.Run(ctx, c); err != nil {
			return err
		}
		if h.cancel != nil {
			h.cancel()
		}
		return errors.New("lost creation acknowledgement or marker write failed")
	}
	if strings.Contains(c.Script, "rm -rf") {
		h.cleanupDetached = ctx.Err() == nil
	}
	return h.Local.Run(ctx, c)
}

func TestCheckCreationFailureCleanupRequiresOwnershipProof(t *testing.T) {
	oldRoot := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = oldRoot })
	for _, kind := range []string{"lost-ack", "cancelled-lost-ack", "collision", "marker-failure"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &stagingFailureHost{kind: kind}
			if kind == "cancelled-lost-ack" {
				h.cancel = cancel
			}
			o := checkOptions(h)
			o.Target = config.BackupTarget{Type: "archive", Repository: t.TempDir()}
			o.Password = ""
			o.ID = "yoho-shop-production-20261013T000000Z.tar.gz"
			_, err := Check(ctx, o)
			if err == nil || h.dir == "" || !strings.Contains(err.Error(), h.dir) || !h.cleanupDetached {
				t.Fatalf("%v dir=%s detached=%v", err, h.dir, h.cleanupDetached)
			}
			if kind == "cancelled-lost-ack" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			_, statErr := os.Stat(h.dir)
			if kind == "lost-ack" || kind == "cancelled-lost-ack" {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("proven staging remains: %v", statErr)
				}
			} else {
				if statErr != nil || !strings.Contains(err.Error(), "remove private check staging") {
					t.Fatalf("unproven staging not preserved/reported: %v stat=%v", err, statErr)
				}
				if _, markerErr := os.Lstat(filepath.Join(h.dir, checkOwnerMarker)); !errors.Is(markerErr, os.ErrNotExist) {
					t.Fatalf("marker written despite failed creation: %v", markerErr)
				}
				if kind == "collision" {
					b, readErr := os.ReadFile(filepath.Join(h.dir, "foreign"))
					if readErr != nil || string(b) != "untouched" {
						t.Fatalf("foreign collision changed: %s %v", b, readErr)
					}
				}
			}
		})
	}
}

func TestRemoveCheckStagingRejectsForeignOrLinkedMarker(t *testing.T) {
	for _, kind := range []string{"wrong-token", "trailing-newline", "symlink-marker", "symlink-dir"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "check-private")
			h := &remote.Local{}
			token := "1234567890abcdef1234567890abcdef"
			if err := createCheckStaging(context.Background(), h, base, dir, token); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, checkOwnerMarker)
			switch kind {
			case "wrong-token":
				if err := os.WriteFile(marker, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing-newline":
				if err := os.WriteFile(marker, []byte(token+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-marker":
				outside := filepath.Join(base, "outside-marker")
				if err := os.WriteFile(outside, []byte(token), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, marker); err != nil {
					t.Fatal(err)
				}
			case "symlink-dir":
				actual := filepath.Join(base, "actual")
				if err := os.Rename(dir, actual); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := removeCheckStaging(context.Background(), h, dir, token); err == nil {
				t.Fatal("removed unproven staging")
			}
			if _, err := os.Lstat(dir); err != nil {
				t.Fatalf("staging changed: %v", err)
			}
		})
	}
}

func TestResticCheckRevalidatesOwnedSnapshotsAfterCheck(t *testing.T) {
	owned := `{"short_id":"abcdef12","tags":["yoho","app=shop","destination=production"]}`
	foreign := `{"short_id":"abcdef34","tags":["yoho","app=other","destination=production"]}`
	for _, tc := range []struct {
		name, post string
		queryErr   error
		healthy    bool
	}{
		{"pruned", "[]", nil, false},
		{"unrelated-only", "[" + foreign + "]", nil, false},
		{"mixed", "[" + foreign + "," + owned + "]", nil, true},
		{"malformed-query", "bad", nil, false},
		{"malformed-entry", "[{}]", nil, false},
		{"query-failed", "", errors.New("post-query test-secret failed"), false},
		{"query-cancelled", "", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lists, checks := 0, 0
			h := &fakeHost{respond: func(ctx context.Context, sc string) (string, error) {
				if strings.Contains(sc, "r snapshots") {
					lists++
					if lists == 1 {
						return "[" + owned + "]", nil
					}
					if checks != 1 {
						t.Fatal("postquery did not follow structural check")
					}
					return tc.post, tc.queryErr
				}
				if strings.Contains(sc, "r check") {
					checks++
				}
				return "", nil
			}}
			_, err := Check(context.Background(), checkOptions(h))
			if (err == nil) != tc.healthy || lists != 2 || checks != 1 {
				t.Fatalf("%v lists=%d checks=%d", err, lists, checks)
			}
			if err != nil && strings.Contains(err.Error(), "test-secret") {
				t.Fatal("postquery leaked secret")
			}
			if tc.name == "query-cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			for _, bad := range []string{"r init", "r restore", "r forget", "mkdir", ".restic-cache"} {
				if strings.Contains(h.scripts(), bad) {
					t.Fatal("check mutated target", h.scripts())
				}
			}
		})
	}
}
