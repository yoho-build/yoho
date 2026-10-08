package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// fakeHost records every command and answers with respond.
type fakeHost struct {
	cmds    []remote.Cmd
	files   map[string][]byte
	modes   map[string]os.FileMode
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

func (f *fakeHost) WriteFile(_ context.Context, p string, data []byte, mode os.FileMode, _ bool) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	if f.modes == nil {
		f.modes = map[string]os.FileMode{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[p] = cp
	f.modes[p] = mode
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
		case strings.HasPrefix(s, "wc -c <"):
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

func TestSwarmRunFindsTaskAndStackVolume(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "volume ls") {
			return "yoho-shop-production_other\nyoho-shop-production_pgdata\n", nil
		}
		return "", nil
	})}
	o := baseOpts(h)
	o.Runtime = "swarm"
	o.Target = config.BackupTarget{Type: "archive", Format: "tar.gz", Repository: "/srv/b", KeepLast: 1}
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	all := h.scripts()
	if strings.Contains(all, "docker compose") {
		t.Fatal("swarm backup used compose")
	}
	if !strings.Contains(all, "label=com.docker.swarm.service.name=yoho-shop-production_db") || !strings.Contains(all, "--filter status=running") {
		t.Fatalf("task lookup:\n%s", all)
	}
	if !strings.Contains(all, "label=com.docker.stack.namespace=yoho-shop-production") {
		t.Fatalf("volume lookup:\n%s", all)
	}
	if !strings.Contains(all, "yoho-shop-production_pgdata:/data:ro") {
		t.Fatal("did not mount the stack volume")
	}
}

func TestSwarmRestoreScalesService(t *testing.T) {
	h := &fakeHost{}
	h.respond = dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "service inspect") {
			return "1", nil
		}
		if strings.Contains(s, "volume ls") {
			return "yoho-shop-production_pgdata\n", nil
		}
		return "", nil
	})
	data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
	m := Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
		"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}},
	}}
	mj, _ := json.Marshal(m)
	h.files = map[string][]byte{data + "/manifest.json": mj}
	_, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h, Confirm: true, Runtime: "swarm",
		ID:     "yoho-shop-production-20261001T030000Z.tar.gz",
		Target: config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{
			"db": {Volumes: []string{"pgdata"}},
		},
		Now: func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	all := h.scripts()
	if strings.Contains(all, "docker compose") {
		t.Fatal("swarm restore used compose")
	}
	inspect := h.index("docker service inspect -f '{{.Spec.Mode.Replicated.Replicas}}' 'yoho-shop-production_db'")
	down := h.index("docker service scale 'yoho-shop-production_db=0'")
	wipe := h.index("find /data -mindepth 1 -delete")
	up := h.index("docker service scale 'yoho-shop-production_db=1'")
	if !(inspect >= 0 && inspect < down && down < wipe && wipe < up) {
		t.Fatalf("bad order inspect=%d down=%d wipe=%d up=%d\n%s", inspect, down, wipe, up, all)
	}
	if !strings.Contains(all, "did not converge") {
		t.Fatal("scale did not wait for convergence")
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

func TestUnknownRuntime(t *testing.T) {
	h := &fakeHost{}
	o := baseOpts(h)
	o.Runtime = "nomad"
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatal(err)
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

func TestListArchivesPOSIXScript(t *testing.T) {
	dir := t.TempDir() + "/my backups"
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{
		"yoho-shop-production-20261001T030000Z.tar.gz": 7,
		"yoho-shop-production-20261008T030000Z.tar.gz": 12,
		"unrelated.txt": 3,
	} {
		if err := os.WriteFile(dir+"/"+name, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var script string
	rec := &recordingLocal{Local: remote.Local{HostName: "local"}, script: &script}
	es, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: rec,
		Target: config.BackupTarget{Type: "archive", Format: "tar.gz", Repository: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Size != 12 || es[1].Size != 7 {
		t.Fatalf("entries %+v", es)
	}
	for _, gnu := range []string{"-printf", "stat -c", "find "} {
		if strings.Contains(script, gnu) {
			t.Errorf("script uses %q:\n%s", gnu, script)
		}
	}
	// Missing directory lists nothing.
	es, err = List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: rec,
		Target: config.BackupTarget{Type: "archive", Format: "tar.gz", Repository: dir + "/missing"}})
	if err != nil || len(es) != 0 {
		t.Fatalf("missing dir: %v %+v", err, es)
	}
}

// recordingLocal runs scripts locally and remembers the last one.
type recordingLocal struct {
	remote.Local
	script *string
}

func (r *recordingLocal) Output(ctx context.Context, c remote.Cmd) (string, error) {
	*r.script = c.Script
	return r.Local.Output(ctx, c)
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
			if strings.Contains(s, "lsf") || strings.Contains(s, "for f in '/srv/b'/") {
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

func TestTargetEncrypts(t *testing.T) {
	cases := []struct {
		cfg  config.BackupTarget
		pass string
		want bool
	}{
		{config.BackupTarget{Repository: "s3:b"}, "pw", true},
		{config.BackupTarget{Type: "restic"}, "", true},
		{config.BackupTarget{Type: "archive", Format: "zip"}, "pw", true},
		{config.BackupTarget{Type: "archive", Format: "7z"}, "pw", true},
		{config.BackupTarget{Type: "archive", Format: "zip"}, "", false},
		{config.BackupTarget{Type: "archive", Format: "7z"}, "", false},
		{config.BackupTarget{Type: "archive", Format: "tar.gz"}, "pw", false},
		{config.BackupTarget{Type: "archive"}, "", false},
	}
	for _, c := range cases {
		if got := TargetEncrypts(c.cfg, c.pass); got != c.want {
			t.Errorf("%+v pass %q: got %v want %v", c.cfg, c.pass, got, c.want)
		}
	}
}

const (
	genPG  = "db-password-from-server"
	genKey = "api-key-from-server"
)

func generatedResponder(extra func(string) (string, error)) func(context.Context, string) (string, error) {
	return dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "/apps/shop/production/generated") {
			return "API_KEY\nPG\nnotes.txt\n", nil
		}
		if strings.Contains(s, "r backup --json") {
			return `{"message_type":"summary","snapshot_id":"abcdef0123","total_bytes_processed":100}`, nil
		}
		if extra != nil {
			return extra(s)
		}
		return "", nil
	})
}

func seedGenerated(h *fakeHost) {
	if h.files == nil {
		h.files = map[string][]byte{}
	}
	dir := "/var/lib/yoho/apps/shop/production/generated"
	h.files[dir+"/PG"] = []byte(genPG)
	h.files[dir+"/API_KEY"] = []byte(genKey)
}

func TestRunIncludesGeneratedSecretsWhenEncrypted(t *testing.T) {
	for _, tg := range []config.BackupTarget{
		{Repository: "s3:shop/backups"},
		{Type: "archive", Format: "zip", Repository: "/srv/b"},
		{Type: "archive", Format: "7z", Repository: "/srv/b"},
	} {
		h := &fakeHost{respond: generatedResponder(nil)}
		seedGenerated(h)
		var out bytes.Buffer
		o := baseOpts(h)
		o.Target = tg
		o.TargetName = "offsite"
		o.Password = secretPW
		o.Out = &out
		if _, err := Run(context.Background(), o); err != nil {
			t.Fatalf("%+v: %v", tg, err)
		}
		staging := "/var/lib/yoho/backups/shop/production/20261008T030000Z/yoho-generated/"
		if string(h.files[staging+"PG"]) != genPG || string(h.files[staging+"API_KEY"]) != genKey {
			t.Fatalf("%+v: staged %#v", tg, h.files)
		}
		if h.modes[staging+"PG"] != 0o600 || h.modes[staging+"API_KEY"] != 0o600 {
			t.Fatalf("%+v: modes %#v", tg, h.modes)
		}
		var m Manifest
		if err := json.Unmarshal(h.files["/var/lib/yoho/backups/shop/production/20261008T030000Z/manifest.json"], &m); err != nil {
			t.Fatal(err)
		}
		if strings.Join(m.GeneratedSecrets, ",") != "API_KEY,PG" {
			t.Fatalf("%+v: manifest names %v", tg, m.GeneratedSecrets)
		}
		blob := out.String() + h.scripts()
		if strings.Contains(blob, genPG) || strings.Contains(blob, genKey) {
			t.Fatal("generated secret value was logged")
		}
	}
}

func TestRunSkipsGeneratedSecretsWhenUnencrypted(t *testing.T) {
	for _, tg := range []config.BackupTarget{
		{Type: "archive", Format: "tar.gz", Repository: "/srv/b"},
		{Type: "archive", Format: "zip", Repository: "/srv/b"},
		{Type: "archive", Format: "7z", Repository: "/srv/b"},
	} {
		h := &fakeHost{respond: generatedResponder(nil)}
		seedGenerated(h)
		var out bytes.Buffer
		o := baseOpts(h)
		o.Target = tg
		o.TargetName = "local"
		o.Out = &out
		if _, err := Run(context.Background(), o); err != nil {
			t.Fatalf("%+v: %v", tg, err)
		}
		for p := range h.files {
			if strings.Contains(p, "yoho-generated") {
				t.Fatalf("%+v: staged %s", tg, p)
			}
		}
		var m Manifest
		if err := json.Unmarshal(h.files["/var/lib/yoho/backups/shop/production/20261008T030000Z/manifest.json"], &m); err != nil {
			t.Fatal(err)
		}
		if len(m.GeneratedSecrets) != 0 {
			t.Fatalf("%+v: manifest %v", tg, m.GeneratedSecrets)
		}
		warn := "generated secrets not included: target local is not encrypted; set a password or use restic"
		if strings.Count(out.String(), warn) != 1 {
			t.Fatalf("%+v: warnings:\n%s", tg, out.String())
		}
		if strings.Contains(out.String(), genPG) || strings.Contains(h.scripts(), genPG) {
			t.Fatal("value logged")
		}
	}
}

func TestRestoreWritesGeneratedSecretsBeforeStart(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(nil)}
	data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
	dst := "/var/lib/yoho/apps/shop/production/generated/PG"
	m := Manifest{
		App: "shop", Destination: "production",
		GeneratedSecrets: []string{"API_KEY", "NEW_TOKEN", "PG"},
		Services: map[string]ManifestService{
			"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}},
		},
	}
	mj, _ := json.Marshal(m)
	h.files = map[string][]byte{
		data + "/manifest.json":            mj,
		data + "/yoho-generated/PG":        []byte("backup-secret-value"),
		data + "/yoho-generated/API_KEY":   []byte("same-api-key"),
		data + "/yoho-generated/NEW_TOKEN": []byte("brand-new-token"),
		dst:                                []byte("old-server-value"),
		"/var/lib/yoho/apps/shop/production/generated/API_KEY": []byte("same-api-key"),
	}
	var out bytes.Buffer
	res, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h, Confirm: true, Out: &out,
		ID:     "yoho-shop-production-20261001T030000Z.tar.gz",
		Target: config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{
			"db": {Volumes: []string{"pgdata"}},
		},
		Now: func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(h.files[dst]) != "backup-secret-value" || h.modes[dst] != 0o600 {
		t.Fatalf("wrote %q mode %o", h.files[dst], h.modes[dst])
	}
	if strings.Join(res.Generated, ",") != "API_KEY,NEW_TOKEN,PG" {
		t.Fatalf("wrote %+v", res.Generated)
	}
	if strings.Join(res.GeneratedChanged, ",") != "PG" {
		t.Fatalf("changed %+v", res.GeneratedChanged)
	}
	if !strings.Contains(out.String(), "generated secret PG differs on the Server; restoring the Backup value") {
		t.Fatalf("warning:\n%s", out.String())
	}
	if strings.Contains(out.String(), "backup-secret-value") || strings.Contains(out.String(), "old-server-value") || strings.Contains(h.scripts(), "backup-secret-value") {
		t.Fatal("secret value was logged")
	}
	wipe := h.index("find /data -mindepth 1 -delete")
	write := h.index("#writefile " + dst)
	start := h.index(" start 'db'")
	if !(wipe >= 0 && wipe < write && write < start) {
		t.Fatalf("order wipe=%d write=%d start=%d\n%s", wipe, write, start, h.scripts())
	}
	for _, c := range h.cmds {
		if strings.HasPrefix(c.Script, "#") {
			continue
		}
		if msg, err := exec.Command("sh", "-n", "-c", c.Script).CombinedOutput(); err != nil {
			t.Errorf("sh -n failed: %v %s\n%s", err, msg, c.Script)
		}
	}

	// Same value: no warning and nothing listed as changed.
	h2 := &fakeHost{respond: dockerResponder(nil)}
	same := []byte("backup-secret-value")
	m.GeneratedSecrets = []string{"PG"}
	mj, _ = json.Marshal(m)
	h2.files = map[string][]byte{
		data + "/manifest.json":     mj,
		data + "/yoho-generated/PG": same,
		dst:                         append([]byte(nil), same...),
	}
	var out2 bytes.Buffer
	res2, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h2, Confirm: true, Out: &out2,
		ID:     "yoho-shop-production-20261001T030000Z.tar.gz",
		Target: config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{
			"db": {Volumes: []string{"pgdata"}},
		},
		Now: func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2.String(), "differs") || len(res2.GeneratedChanged) != 0 {
		t.Fatalf("same value must not warn: %+v\n%s", res2.GeneratedChanged, out2.String())
	}
}

func TestEncryptedArchiveExtractOwnsTreeAndReportsMissingManifest(t *testing.T) {
	for _, format := range []string{"zip", "7z"} {
		t.Run(format, func(t *testing.T) {
			h := &fakeHost{respond: dockerResponder(nil)}
			data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
			m := Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
				"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}},
			}}
			mj, _ := json.Marshal(m)
			h.files = map[string][]byte{data + "/manifest.json": mj}
			id := "yoho-shop-production-20261001T030000Z." + format
			if _, err := Restore(context.Background(), RestoreOptions{
				App: "shop", Destination: "production", Host: h, Confirm: true,
				ID: id, Password: secretPW,
				Target:   config.BackupTarget{Type: "archive", Format: format, Repository: "/srv/b"},
				Services: map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}}},
				Now:      func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
			}); err != nil {
				t.Fatal(err)
			}
			i := -1
			var script string
			for n, c := range h.cmds {
				if strings.Contains(c.Script, "chown -R") && strings.Contains(c.Script, "-o/s/data") {
					i = n
					script = c.Script
					break
				}
			}
			if i < 0 {
				t.Fatalf("extract script missing:\n%s", h.scripts())
			}
			if !strings.Contains(script, `uid=$(id -u)`) || !strings.Contains(script, `gid=$(id -g)`) || !strings.Contains(script, `chown -R "$1:$2" /s/data`) || !strings.Contains(script, `chmod -R u+rwX,go-rwx /s/data`) {
				t.Fatalf("extract does not hand the tree to the deploy user:\n%s", script)
			}
			if !strings.Contains(script, `-p"$YOHO_ARCHIVE_PASSWORD"`) || strings.Contains(script, secretPW) {
				t.Fatal("password must be expanded inside the extractor, not written into the script")
			}
			if c := h.cmds[i]; c.Env[envArchivePass] != secretPW {
				t.Fatalf("password env = %q", c.Env[envArchivePass])
			}
			if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("sh -n: %v %s\n%s", err, out, script)
			}
		})
	}

	h := &fakeHost{respond: dockerResponder(nil)}
	_, err := Restore(context.Background(), RestoreOptions{
		App: "shop", Destination: "production", Host: h, Confirm: true,
		ID: "yoho-shop-production-20261001T030000Z.zip", Password: secretPW,
		Target:   config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}}},
		Now:      func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	})
	if err == nil || !strings.Contains(err.Error(), "manifest.json missing or not readable") {
		t.Fatalf("err = %v", err)
	}
}

func TestGeneratedStagingRemovedAfterStore(t *testing.T) {
	const staging = "/var/lib/yoho/backups/shop/production/20261008T030000Z"
	staged := staging + "/yoho-generated/PG"
	cleanup := "rm -rf '" + staging + "'"

	success := func(t *testing.T, tg config.BackupTarget, storeMarker string) {
		t.Helper()
		h := &fakeHost{respond: generatedResponder(nil)}
		seedGenerated(h)
		o := baseOpts(h)
		o.Target = tg
		o.Password = secretPW
		o.TargetName = "offsite"
		if _, err := Run(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		write, store, clean := h.index("#writefile "+staged), h.index(storeMarker), h.index(cleanup)
		if !(write >= 0 && write < store && store < clean) {
			t.Fatalf("order write=%d store=%d cleanup=%d\n%s", write, store, clean, h.scripts())
		}
	}
	success(t, config.BackupTarget{Repository: "s3:shop/backups"}, "r backup --json")
	success(t, config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/b"}, "-tzip")

	fail := func(t *testing.T, tg config.BackupTarget, marker, boom string) {
		t.Helper()
		h := &fakeHost{respond: dockerResponder(func(s string) (string, error) {
			if strings.Contains(s, "/apps/shop/production/generated") {
				return "PG\n", nil
			}
			if strings.Contains(s, marker) {
				return "", errors.New(boom)
			}
			return "", nil
		})}
		seedGenerated(h)
		o := baseOpts(h)
		o.Target = tg
		o.Password = secretPW
		if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), boom) {
			t.Fatalf("err = %v", err)
		}
		store, clean := h.index(marker), h.index(cleanup)
		if !(store >= 0 && store < clean) {
			t.Fatalf("cleanup did not follow failed store: store=%d cleanup=%d\n%s", store, clean, h.scripts())
		}
		if h.index("#writefile "+staged) > store {
			t.Fatal("secrets staged after the store step")
		}
	}
	fail(t, config.BackupTarget{Repository: "s3:shop/backups"}, "r backup --json", "repo down")
	fail(t, config.BackupTarget{Type: "archive", Format: "7z", Repository: "/srv/b"}, "-t7z", "disk full")
}

func TestListGeneratedAndNextDeployKeepsRestoredValue(t *testing.T) {
	root := t.TempDir()
	old := release.Root
	release.Root = root
	t.Cleanup(func() { release.Root = old })

	dir := filepath.Join(root, "apps", "shop", "production", "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG"), []byte("kept-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "not-a-file"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := &remote.Local{}
	names, err := ListGenerated(context.Background(), h, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "PG" {
		t.Fatalf("names %v", names)
	}
	got, err := deploy.EnsureGenerated(context.Background(), h, release.AppDir("shop", "production"), map[string]string{"PG": "password32"})
	if err != nil {
		t.Fatal(err)
	}
	if got["PG"] != "kept-value" {
		t.Fatalf("next deploy replaced the restored secret: %q", got["PG"])
	}
	fi, err := os.Stat(filepath.Join(dir, "PG"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}

func TestRunPausesEveryContainerMountingTheVolume(t *testing.T) {
	base := dockerResponder(nil)
	h := &fakeHost{respond: func(ctx context.Context, s string) (string, error) {
		if strings.Contains(s, "docker ps -q --filter 'volume=") {
			return "cid123\nother99\nworker77\n", nil
		}
		return base(ctx, s)
	}}
	o := baseOpts(h)
	o.Target = config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/backups", PasswordSecret: "PW"}
	o.Password = secretPW
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cp := h.index("tar -C /data -czf")
	for _, id := range []string{"cid123", "other99", "worker77"} {
		p, u := h.index("docker pause '"+id+"'"), h.index("docker unpause '"+id+"'")
		if p < 0 || u < 0 || !(p < cp && cp < u) {
			t.Errorf("%s: pause=%d copy=%d unpause=%d\n%s", id, p, cp, u, h.scripts())
		}
	}
	if n := strings.Count(h.scripts(), "docker pause 'cid123'"); n != 1 {
		t.Errorf("Service container paused %d times", n)
	}
}

func TestRunHandsStagedArchivesToDeployUser(t *testing.T) {
	h := &fakeHost{respond: dockerResponder(nil)}
	o := baseOpts(h)
	o.Target = config.BackupTarget{Type: "archive", Format: "zip", Repository: "/srv/backups", PasswordSecret: "PW"}
	o.Password = secretPW
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	i := h.index("tar -C /data -czf")
	if i < 0 {
		t.Fatal("no copy")
	}
	s := h.cmds[i].Script
	if !strings.Contains(s, `chown "$2:$3"`) || !strings.Contains(s, `"$(id -u)" "$(id -g)"`) || !strings.Contains(s, "umask 077") {
		t.Errorf("archive must stay 0600 and be chowned to the deploy user:\n%s", s)
	}
}
