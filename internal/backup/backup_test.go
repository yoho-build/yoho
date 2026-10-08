package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
)

// fakeHost records every command and answers with respond.
type fakeHost struct {
	cmds    []remote.Cmd
	files   map[string][]byte
	respond func(ctx context.Context, script string) (string, error)
}

func (f *fakeHost) Name() string { return "fake" }

func (f *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}

func (f *fakeHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	f.cmds = append(f.cmds, c)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.respond != nil {
		return f.respond(ctx, c.Script)
	}
	return "", nil
}

func (f *fakeHost) WriteFile(_ context.Context, p string, data []byte, _ os.FileMode, _ bool) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[p] = data
	f.cmds = append(f.cmds, remote.Cmd{Script: "#writefile " + p})
	return nil
}

func (f *fakeHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	if b, ok := f.files[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) scripts() string {
	var b strings.Builder
	for _, c := range f.cmds {
		b.WriteString(c.Script + "\n---\n")
	}
	return b.String()
}

func (f *fakeHost) index(sub string) int {
	for i, c := range f.cmds {
		if strings.Contains(c.Script, sub) {
			return i
		}
	}
	return -1
}

// dockerResponder answers container and volume lookups.
func dockerResponder(extra func(string) (string, error)) func(context.Context, string) (string, error) {
	return func(_ context.Context, s string) (string, error) {
		switch {
		case strings.Contains(s, " ps -q "):
			return "cid123", nil
		case strings.HasPrefix(s, "docker volume ls"):
			return "yoho-shop-production_pgdata", nil
		case strings.HasPrefix(s, "stat -c"):
			return "4096", nil
		}
		if extra != nil {
			return extra(s)
		}
		return "", nil
	}
}

const secretPW = "s3cr3t-Pa55w0rd!"

func baseOpts(h remote.Host) RunOptions {
	return RunOptions{
		App: "shop", Destination: "production", Host: h,
		Services: map[string]config.ServiceBackup{
			"db": {Volumes: []string{"pgdata"}, Dump: []string{"pg_dump", "-U", "app", "app"}},
		},
		Now: func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	}
}

func TestRunArchiveZipPasswordNeverInScript(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(nil)}
	o := baseOpts(h)
	o.Target = config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/backups", PasswordSecret: "PW", KeepLast: 3}
	o.Password = secretPW
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "yoho-shop-production-20261008T030000Z.zip" || res.Method != "archive:zip" || res.Size != 4096 {
		t.Fatalf("result %+v", res)
	}
	all := h.scripts()
	if strings.Contains(all, secretPW) {
		t.Fatal("password appears in a script")
	}
	if !strings.Contains(all, "-mem=AES256") || strings.Contains(all, "ZipCrypto") {
		t.Fatal("zip must use AES-256")
	}
	found := false
	for _, c := range h.cmds {
		if c.Env[envArchivePass] == secretPW {
			found = true
		}
	}
	if !found {
		t.Fatal("password not passed via env")
	}
	// Order: dump before pause, pause before copy, unpause after copy, cleanup last.
	dump, pause, cp, unpause := h.index("pg_dump"), h.index("docker pause"), h.index("tar -C /data -czf"), h.index("docker unpause")
	if !(dump >= 0 && dump < pause && pause < cp && cp < unpause) {
		t.Fatalf("bad order dump=%d pause=%d copy=%d unpause=%d\n%s", dump, pause, cp, unpause, all)
	}
	if h.index("rm -rf '/var/lib/yoho/backups/shop/production/20261008T030000Z'") < 0 {
		t.Fatal("staging not cleaned up")
	}
	var m Manifest
	if err := json.Unmarshal(h.files["/var/lib/yoho/backups/shop/production/20261008T030000Z/manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Services["db"].Volumes["pgdata"] != "db-pgdata.tar.gz" || m.Services["db"].Dump != "db.dump" || m.Services["db"].Quiesce != "pause" {
		t.Fatalf("manifest %+v", m)
	}
}

func TestRunUnpausesAndCleansUpOnCopyError(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "tar -C /data -czf") {
			return "", errors.New("disk full")
		}
		return "", nil
	})}
	o := baseOpts(h)
	o.Target = config.BackupTarget{Repository: "/srv/restic"}
	o.Password = secretPW
	_, err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if h.index("docker unpause") < h.index("tar -C /data") {
		t.Fatal("unpause not executed after failed copy")
	}
	if h.index("rm -rf ") < 0 {
		t.Fatal("staging not cleaned up")
	}
	if h.index("restic") >= 0 {
		t.Fatal("must not store after failure")
	}
}

func TestRunUnpausesWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &fakeHost{}
	h.respond = dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "docker pause") {
			cancel() // operator hits Ctrl-C while paused
		}
		return "", nil
	})
	o := baseOpts(h)
	o.Target = config.BackupTarget{Repository: "/srv/restic"}
	o.Password = secretPW
	if _, err := Run(ctx, o); err == nil {
		t.Fatal("want error")
	}
	i := h.index("docker unpause")
	if i < 0 {
		t.Fatal("unpause not attempted")
	}
	if h.index("rm -rf ") < i {
		t.Fatal("staging cleanup missing")
	}
}

func TestRunPreBackupSkipsPause(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "r backup --json") {
			return `{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"abcdef0123456789","total_bytes_processed":1234}`, nil
		}
		return "", nil
	})}
	o := baseOpts(h)
	o.Services = map[string]config.ServiceBackup{"app": {Volumes: []string{"storage"}, PreBackup: []string{"/hooks/pre-backup"}}}
	o.Target = config.BackupTarget{Repository: "s3:s3.amazonaws.com/bucket", KeepLast: 7, EnvSecrets: []string{"AWS_SECRET_ACCESS_KEY"}}
	o.Password = secretPW
	o.TargetEnv = map[string]string{"AWS_SECRET_ACCESS_KEY": "awsSECRETvalue"}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "abcdef01" || res.Size != 1234 || res.Method != "restic" {
		t.Fatalf("result %+v", res)
	}
	all := h.scripts()
	if strings.Contains(all, "docker pause") || h.index("/hooks/pre-backup") < 0 {
		t.Fatal("pre_backup must replace pause")
	}
	if strings.Contains(all, secretPW) || strings.Contains(all, "awsSECRETvalue") {
		t.Fatal("secret in script")
	}
	if !strings.Contains(all, "-e AWS_SECRET_ACCESS_KEY") || !strings.Contains(all, "-e RESTIC_PASSWORD") {
		t.Fatal("env not forwarded to restic container")
	}
	if !strings.Contains(all, "--group-by tags --keep-last 7 --prune") {
		t.Fatal("retention missing")
	}
}

func TestTargetValidation(t *testing.T) {
	cases := []struct {
		cfg config.BackupTarget
		pw  string
	}{
		{config.BackupTarget{Type: "archive", Repository: "/b"}, "pw"},         // tar.gz + password
		{config.BackupTarget{Repository: "/b"}, ""},                            // restic without password
		{config.BackupTarget{Type: "archive", Repository: "relative/dir"}, ""}, // relative path
		{config.BackupTarget{Type: "archive", Format: "rar", Repository: "/b"}, ""},
		{config.BackupTarget{Repository: "/b", EnvSecrets: []string{"MISSING"}}, "pw"},
	}
	for i, c := range cases {
		if _, err := newTarget("a", "b", "yoho-a-b", c.cfg, c.pw, nil); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
}

func TestListArchivesRclone(t *testing.T) {
	h := &fakeHost{respond: func(_ context.Context, s string) (string, error) {
		return "yoho-shop-production-20261001T030000Z.7z 10\nother.txt 5\nyoho-shop-production-20261008T030000Z.7z 20\nyoho-shop-staging-20261008T030000Z.7z 1", nil
	}}
	es, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: h,
		Target: config.BackupTarget{Type: "archive", Format: "7z", Repository: "onedrive:backups"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Size != 20 || !es[0].Time.After(es[1].Time) {
		t.Fatalf("entries %+v", es)
	}
}

func TestRestoreRequiresConfirm(t *testing.T) {
	h := &fakeHost{}
	_, err := Restore(context.Background(), RestoreOptions{App: "shop", Destination: "production", Host: h, ID: "latest"})
	if err == nil || len(h.cmds) != 0 {
		t.Fatal("restore without Confirm must fail before touching the Server")
	}
}

func TestRestoreArchive(t *testing.T) {
	h := &fakeHost{}
	h.respond = dockerResponder(nil)
	data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
	m := Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
		"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}, Dump: "db.dump", Quiesce: "pause"},
	}}
	mj, _ := json.Marshal(m)
	h.files = map[string][]byte{data + "/manifest.json": mj}
	res, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h, Confirm: true,
		ID:       "yoho-shop-production-20261001T030000Z.zip",
		Target:   config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/b"},
		Password: secretPW,
		Services: map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}, PostRestore: []string{"/hooks/post-restore"}}},
		Now:      func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	stop, wipe, start, post := h.index(" stop 'db'"), h.index("find /data -mindepth 1 -delete"), h.index(" start 'db'"), h.index("/hooks/post-restore")
	if !(stop >= 0 && stop < wipe && wipe < start && start < post) {
		t.Fatalf("bad order\n%s", h.scripts())
	}
	if strings.Contains(h.scripts(), secretPW) {
		t.Fatal("password in script")
	}
	if len(res.DumpFiles) != 1 || res.StagingDir == "" || h.index("rm -rf ") >= 0 {
		t.Fatalf("dumps must be kept: %+v", res)
	}
}

// TestScriptsParse runs `sh -n` on every script a Backup generates.
func TestScriptsParse(t *testing.T) {
	for _, tg := range []config.BackupTarget{
		{Repository: "/srv/restic", KeepLast: 2},
		{Type: "archive", Format: "tar.gz", Repository: "/srv/b", KeepLast: 2},
		{Type: "archive", Format: "7z", Repository: "gdrive:yoho", KeepLast: 2},
	} {
		h := &fakeHost{respond: dockerResponder(func(s string) (string, error) {
			if strings.Contains(s, "r backup --json") {
				return `{"message_type":"summary","snapshot_id":"abcdef0123"}`, nil
			}
			if strings.Contains(s, "lsf") || strings.Contains(s, "find '/srv/b'") {
				return "yoho-shop-production-20261001T030000Z.7z 1\nyoho-shop-production-20261002T030000Z.7z 1\nyoho-shop-production-20261003T030000Z.7z 1\n" +
					"yoho-shop-production-20261001T030000Z.tar.gz 1\nyoho-shop-production-20261002T030000Z.tar.gz 1\nyoho-shop-production-20261003T030000Z.tar.gz 1", nil
			}
			return "", nil
		})}
		o := baseOpts(h)
		o.Target = tg
		if tg.Type == "" || tg.Format == "7z" {
			o.Password = secretPW
		}
		if _, err := Run(context.Background(), o); err != nil {
			t.Fatalf("%+v: %v", tg, err)
		}
		if !strings.Contains(h.scripts(), "forget") && !strings.Contains(h.scripts(), "20261001T030000Z") {
			t.Errorf("%+v: retention did not delete the oldest", tg)
		}
		for _, c := range h.cmds {
			if strings.HasPrefix(c.Script, "#") {
				continue
			}
			if out, err := exec.Command("sh", "-n", "-c", c.Script).CombinedOutput(); err != nil {
				t.Errorf("sh -n failed: %v %s\n%s", err, out, c.Script)
			}
		}
	}
}

func TestRestoreDumpAfterStart(t *testing.T) {
	h := &fakeHost{}
	h.respond = dockerResponder(nil)
	data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
	m := Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
		"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}, Dump: "db.dump", Quiesce: "pause"},
	}}
	mj, _ := json.Marshal(m)
	h.files = map[string][]byte{data + "/manifest.json": mj}
	res, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h, Confirm: true,
		ID:     "yoho-shop-production-20261001T030000Z.tar.gz",
		Target: config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{
			"db":    {Volumes: []string{"pgdata"}, RestoreDump: []string{"psql", "-U", "app"}, PostRestore: []string{"/post"}},
			"cache": {Volumes: []string{"redis"}},
		},
		Now: func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	start, ready, load, post := h.index(" start 'db'"), h.index("State.Health"), h.index("docker exec -i 'cid123' 'psql' '-U' 'app' < '"+data+"/db.dump'"), h.index("'/post'")
	if !(start >= 0 && start < ready && ready < load && load < post) {
		t.Fatalf("bad order %d %d %d %d\n%s", start, ready, load, post, h.scripts())
	}
	if len(res.Dumps) != 1 || len(res.DumpFiles) != 0 || len(res.Skipped) != 1 || res.Skipped[0] != "cache" {
		t.Fatalf("result %+v", res)
	}
	if h.index("rm -rf ") < 0 {
		t.Fatal("staging dir must be removed when every dump was loaded")
	}
	for _, c := range h.cmds {
		if strings.HasPrefix(c.Script, "#") {
			continue
		}
		if out, err := exec.Command("sh", "-n", "-c", c.Script).CombinedOutput(); err != nil {
			t.Errorf("sh -n failed: %v %s\n%s", err, out, c.Script)
		}
	}
}
