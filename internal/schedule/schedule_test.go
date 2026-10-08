package schedule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

type fakeHost struct {
	cmds    []remote.Cmd
	files   map[string][]byte
	respond func(script string) (string, error)
}

func (f *fakeHost) Name() string { return "fake" }
func (f *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}
func (f *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	f.cmds = append(f.cmds, c)
	if f.respond != nil {
		return f.respond(c.Script)
	}
	return "", nil
}
func (f *fakeHost) WriteFile(_ context.Context, p string, data []byte, _ os.FileMode, _ bool) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[p] = append([]byte(nil), data...)
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
		b.WriteString(c.Script + "\n")
	}
	return b.String()
}

const pw = "very-secret-pw"

func testJob() Job {
	return Job{
		App: "shop", Destination: "production", Name: "nightly",
		Services:   map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}}},
		TargetName: "offsite",
		Target:     config.BackupTarget{Repository: "s3:s3.amazonaws.com/b", PasswordSecret: "BACKUP_PASSWORD", EnvSecrets: []string{"AWS_SECRET_ACCESS_KEY"}},
		Schedule:   "*-*-* 03:00:00",
		Secrets:    map[string]string{"BACKUP_PASSWORD": pw, "AWS_SECRET_ACCESS_KEY": "aws-secret"},
	}
}

func TestRenderSystemUnits(t *testing.T) {
	s, err := NewSpec(testJob())
	if err != nil {
		t.Fatal(err)
	}
	svc := RenderService(s, Mode{Sudo: true})
	for _, want := range []string{
		"Type=oneshot\n", "User=yoho\n",
		"ExecStart=/usr/local/libexec/yoho schedule run --job /var/lib/yoho/jobs/shop-production-nightly.json\n",
		"TimeoutStartSec=21600\n", "Nice=10\n", "After=network-online.target docker.service\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service missing %q:\n%s", want, svc)
		}
	}
	tm := RenderTimer(s)
	for _, want := range []string{"OnCalendar=*-*-* 03:00:00\n", "Persistent=true\n", "RandomizedDelaySec=300\n", "WantedBy=timers.target\n"} {
		if !strings.Contains(tm, want) {
			t.Errorf("timer missing %q:\n%s", want, tm)
		}
	}
}

func TestRenderUserUnits(t *testing.T) {
	s, _ := NewSpec(testJob())
	svc := RenderService(s, Mode{})
	if strings.Contains(svc, "User=") || strings.Contains(svc, "docker.service") {
		t.Fatalf("user unit must not set User= or system deps:\n%s", svc)
	}
	if !strings.Contains(svc, "ExecStart=/var/lib/yoho/bin/yoho schedule run") {
		t.Fatalf("user binary path:\n%s", svc)
	}
}

func TestNewSpecValidation(t *testing.T) {
	j := testJob()
	j.Schedule = "daily\nExecStart=/bin/evil"
	if _, err := NewSpec(j); err == nil {
		t.Error("newline in schedule accepted")
	}
	j = testJob()
	delete(j.Secrets, "BACKUP_PASSWORD")
	if _, err := NewSpec(j); err == nil {
		t.Error("missing secret accepted")
	}
	j = testJob()
	j.Name = "../x"
	if _, err := NewSpec(j); err == nil {
		t.Error("bad name accepted")
	}
}

func TestSpecRoundTripHasNoSecretValues(t *testing.T) {
	s, _ := NewSpec(testJob())
	b, _ := json.Marshal(s)
	if bytes.Contains(b, []byte(pw)) || bytes.Contains(b, []byte("aws-secret")) {
		t.Fatal("secret value in spec")
	}
	var back Spec
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SecretFiles["BACKUP_PASSWORD"] != "/var/lib/yoho/apps/shop/production/scheduled/BACKUP_PASSWORD" ||
		back.Services["db"].Volumes[0] != "pgdata" || back.Target.Repository != s.Target.Repository {
		t.Fatalf("round trip: %+v", back)
	}
}

func writeBinary(t *testing.T) (string, string) {
	p := filepath.Join(t.TempDir(), "yoho")
	data := []byte("fake binary")
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return p, hex.EncodeToString(sum[:])
}

func TestInstallSystem(t *testing.T) {
	bin, sum := writeBinary(t)
	h := &fakeHost{}
	uploaded := false
	h.respond = func(s string) (string, error) {
		if strings.HasPrefix(s, "sha256sum") && uploaded {
			return sum, nil
		}
		if strings.HasPrefix(s, "mkdir -p '/usr/local/libexec'") {
			uploaded = true
		}
		return "", nil
	}
	var out bytes.Buffer
	res, err := Install(context.Background(), h, InstallOptions{Jobs: []Job{testJob()}, YohoBinaryLocalPath: bin, Mode: Mode{Sudo: true, User: "deploy"}, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.scripts(), pw) || strings.Contains(out.String(), pw) {
		t.Fatal("secret value in scripts or output")
	}
	for _, c := range h.cmds {
		if len(c.Env) > 0 {
			t.Fatal("install must not pass env")
		}
	}
	if string(h.files["/var/lib/yoho/apps/shop/production/scheduled/BACKUP_PASSWORD"]) != pw {
		t.Fatal("secret file not written")
	}
	if _, ok := h.files["/etc/systemd/system/yoho-shop-production-nightly.timer"]; !ok {
		t.Fatal("timer not written")
	}
	all := h.scripts()
	for _, want := range []string{"systemd-analyze calendar '*-*-* 03:00:00'", "chown 'deploy:'", "systemctl daemon-reload", "systemctl enable --now 'yoho-shop-production-nightly.timer'"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func TestInstallUserWarnsWithoutLinger(t *testing.T) {
	bin, sum := writeBinary(t)
	h := &fakeHost{respond: func(s string) (string, error) {
		switch {
		case strings.Contains(s, "$HOME"):
			return "/home/cub", nil
		case strings.HasPrefix(s, "sha256sum"):
			return sum, nil // already present: no upload
		case strings.Contains(s, "/var/lib/systemd/linger/"):
			return "off cub", nil
		}
		return "", nil
	}}
	res, err := Install(context.Background(), h, InstallOptions{Jobs: []Job{testJob()}, YohoBinaryLocalPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.files["/home/cub/.config/systemd/user/yoho-shop-production-nightly.service"]; !ok {
		t.Fatal("user unit not written")
	}
	if _, ok := h.files["/var/lib/yoho/bin/yoho"]; ok {
		t.Fatal("binary re-uploaded despite matching checksum")
	}
	if !strings.Contains(h.scripts(), "systemctl --user enable --now") {
		t.Fatal("user timers not enabled")
	}
	for _, c := range h.cmds {
		if c.Sudo {
			t.Fatal("user mode must not use sudo")
		}
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "sudo loginctl enable-linger cub") || !strings.Contains(res.Warnings[0], "yoho setup") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func TestRunJobRecordsFailure(t *testing.T) {
	s, _ := NewSpec(testJob())
	sp := filepath.Join(t.TempDir(), "job.json")
	b, _ := json.Marshal(s)
	if err := os.WriteFile(sp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{files: map[string][]byte{
		s.SecretFiles["BACKUP_PASSWORD"]:       []byte(pw),
		s.SecretFiles["AWS_SECRET_ACCESS_KEY"]: []byte("aws-secret"),
	}}
	h.respond = func(sc string) (string, error) {
		if strings.Contains(sc, " ps -q ") {
			return "", errors.New("docker down")
		}
		return "", nil
	}
	now := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	err := RunJob(context.Background(), sp, RunJobOptions{Host: h, Now: func() time.Time { return now }})
	if err == nil {
		t.Fatal("want error")
	}
	var states map[string]JobState
	if err := json.Unmarshal(h.files[StatePath("shop", "production")], &states); err != nil {
		t.Fatal(err)
	}
	st := states["nightly"]
	if !st.LastRun.Equal(now) || st.LastSuccess != nil || !strings.Contains(st.Error, "docker down") {
		t.Fatalf("state %+v", st)
	}
	if strings.Contains(string(h.files[StatePath("shop", "production")]), pw) {
		t.Fatal("secret in state")
	}
}

func TestStatusParsesSystemd(t *testing.T) {
	s, _ := NewSpec(testJob())
	b, _ := json.Marshal(s)
	h := &fakeHost{files: map[string][]byte{
		"/var/lib/yoho/jobs/shop-production-nightly.json": b,
		StatePath("shop", "production"):                   []byte(`{"nightly":{"last_run":"2026-10-08T03:00:00Z","error":"boom"}}`),
	}}
	h.respond = func(sc string) (string, error) {
		if strings.HasPrefix(sc, "ls -1") {
			return "shop-production-nightly.json\nshop-production-x-other.txt", nil
		}
		if strings.Contains(sc, " show ") {
			return "ActiveState=active\nNextElapseUSecRealtime=Fri 2026-10-09 03:00:00 UTC\nLastTriggerUSec=n/a\nResult=exit-code", nil
		}
		return "", nil
	}
	st, err := Status(context.Background(), h, "shop", "production", Mode{Sudo: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].TimerState != "active" || st[0].ServiceResult != "exit-code" || st[0].State.Error != "boom" {
		t.Fatalf("status %+v", st)
	}
}

func TestRunJobUsesSpecRuntime(t *testing.T) {
	for _, tc := range []struct{ runtime, want string }{
		{"swarm", "com.docker.swarm.service.name"},
		{"", "docker compose -p"}, // specs written before Runtime existed
		{"compose", "docker compose -p"},
	} {
		j := testJob()
		j.Runtime = tc.runtime
		s, err := NewSpec(j)
		if err != nil {
			t.Fatal(err)
		}
		if tc.runtime == "compose" && s.Runtime != "" {
			t.Errorf("compose is the default and is not serialized: %q", s.Runtime)
		}
		sp := filepath.Join(t.TempDir(), "job.json")
		b, _ := json.Marshal(s)
		if err := os.WriteFile(sp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		h := &fakeHost{files: map[string][]byte{
			s.SecretFiles["BACKUP_PASSWORD"]:       []byte(pw),
			s.SecretFiles["AWS_SECRET_ACCESS_KEY"]: []byte("aws-secret"),
		}}
		h.respond = func(sc string) (string, error) {
			if strings.Contains(sc, " ps -q") || strings.Contains(sc, "docker ps -q") {
				return "", errors.New("stop here")
			}
			return "", nil
		}
		_ = RunJob(context.Background(), sp, RunJobOptions{Host: h})
		if !strings.Contains(h.scripts(), tc.want) {
			t.Errorf("runtime %q: want %q in scripts:\n%s", tc.runtime, tc.want, h.scripts())
		}
	}
	j := testJob()
	j.Runtime = "nomad"
	if _, err := NewSpec(j); err == nil {
		t.Error("unknown runtime must be rejected")
	}
}

func TestSpecCarriesSecretFingerprintsNotValues(t *testing.T) {
	j := testJob()
	j.SecretFingerprints = map[string]string{"BACKUP_PASSWORD": "aaaa", "AWS_SECRET_ACCESS_KEY": "bbbb", "UNRELATED": "cccc"}
	s, err := NewSpec(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SecretFingerprints) != 2 || s.SecretFingerprints["AWS_SECRET_ACCESS_KEY"] != "bbbb" {
		t.Fatalf("%+v", s.SecretFingerprints)
	}
}
