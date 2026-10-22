package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

const (
	pullOld = "yoho-shop-production-20261001T030000Z.tar.gz"
	pullNew = "yoho-shop-production-20261008T030000Z.tar.gz"
)

// pullRepo makes a Server-side archive directory with two archives plus
// files Pull must ignore.
func pullRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		pullOld:             "old archive",
		pullNew:             "newer archive body",
		"unrelated.txt":     "x",
		".yoho-publish-abc": "half",
		"yoho-shop-production-20261009T030000Z.zip": "wrong format",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pullOpts(repo, local string) PullOptions {
	return PullOptions{
		App: "shop", Destination: "production", Server: "primary", Dir: local,
		Host:   &remote.Local{HostName: "primary"},
		Target: config.BackupTarget{Type: "archive", Format: "tar.gz", Repository: repo},
	}
}

func statuses(rs []PulledArchive) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		m[r.ID] = r.Status
	}
	return m
}

func TestPullCopiesThenSkips(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	rs, err := Pull(context.Background(), pullOpts(repo, local))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].ID != pullOld || rs[1].ID != pullNew {
		t.Fatalf("want oldest first, only matching archives: %+v", rs)
	}
	dir := filepath.Join(local, "shop", "production")
	b, err := os.ReadFile(filepath.Join(dir, pullNew))
	if err != nil || string(b) != "newer archive body" {
		t.Fatalf("copy: %q %v", b, err)
	}
	sum := sha256.Sum256(b)
	if rs[1].Status != PullCopied || rs[1].SHA256 != hex.EncodeToString(sum[:]) || rs[1].Size != int64(len(b)) {
		t.Fatalf("result %+v", rs[1])
	}
	if fi, _ := os.Stat(filepath.Join(dir, pullNew)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("only the two archives (no .part, no unrelated): %v", entries)
	}
	rs, err = Pull(context.Background(), pullOpts(repo, local))
	if err != nil || statuses(rs)[pullOld] != PullSkipped || statuses(rs)[pullNew] != PullSkipped {
		t.Fatalf("second run must skip: %+v %v", rs, err)
	}
}

func TestPullConflictNeverOverwrites(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	dir := filepath.Join(local, "shop", "production")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pullOld), []byte("different length contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := Pull(context.Background(), pullOpts(repo, local))
	if err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Fatalf("want conflict error, got %v", err)
	}
	if st := statuses(rs); st[pullOld] != PullConflict || st[pullNew] != PullCopied {
		t.Fatalf("statuses %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, pullOld)); string(b) != "different length contents" {
		t.Fatal("conflicting local file was overwritten")
	}
}

func TestPullNeverDeletesLocalFiles(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	dir := filepath.Join(local, "shop", "production")
	os.MkdirAll(dir, 0o700)
	keep := filepath.Join(dir, "yoho-shop-production-20200101T000000Z.tar.gz") // pruned on the Server long ago
	os.WriteFile(keep, []byte("keep me"), 0o600)
	if _, err := Pull(context.Background(), pullOpts(repo, local)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("local archive removed: %v", err)
	}
}

type corrupt struct{ w io.Writer }

func (c corrupt) Write(p []byte) (int, error) {
	q := bytes.Clone(p)
	if len(q) > 0 {
		q[0] ^= 0xff
	}
	return c.w.Write(q)
}

func TestPullChecksumMismatchRemovesPart(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	o := pullOpts(repo, local)
	o.wrap = func(w io.Writer) io.Writer { return corrupt{w} }
	rs, err := Pull(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	for _, r := range rs {
		if r.Status != PullFailed {
			t.Fatalf("%+v", r)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(local, "shop", "production"))
	if len(entries) != 0 {
		t.Fatalf("no .part or archive may remain: %v", entries)
	}
}

func TestPullLimitKeepsNewest(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	o := pullOpts(repo, local)
	o.Limit = 1
	rs, err := Pull(context.Background(), o)
	if err != nil || len(rs) != 1 || rs[0].ID != pullNew {
		t.Fatalf("%+v %v", rs, err)
	}
}

func TestPullServerSubdir(t *testing.T) {
	repo, local := pullRepo(t), t.TempDir()
	o := pullOpts(repo, local)
	o.ServerSubdir = true
	if _, err := Pull(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, "shop", "production", "primary", pullNew)); err != nil {
		t.Fatal(err)
	}
}

// shadowTools defines shell functions that make named commands fail, as on
// a Server lacking them.
type shadowTools struct {
	remote.Host
	tools []string
}

func (s shadowTools) Output(ctx context.Context, c remote.Cmd) (string, error) {
	for _, tool := range s.tools {
		c.Script = tool + "() { return 127; }\n" + c.Script
	}
	return s.Host.Output(ctx, c)
}

func TestPullHashFallbacks(t *testing.T) {
	for _, tools := range [][]string{{"sha256sum"}, {"sha256sum", "shasum"}} {
		repo, local := pullRepo(t), t.TempDir()
		o := pullOpts(repo, local)
		o.Host = shadowTools{Host: o.Host, tools: tools}
		if _, err := Pull(context.Background(), o); err != nil {
			t.Fatalf("without %v: %v", tools, err)
		}
	}
	repo, local := pullRepo(t), t.TempDir()
	o := pullOpts(repo, local)
	o.Host = shadowTools{Host: o.Host, tools: []string{"sha256sum", "shasum", "openssl"}}
	if _, err := Pull(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no SHA-256 tool") {
		t.Fatalf("want missing tool error, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(local, "shop", "production"))
	if len(entries) != 0 {
		t.Fatalf("unverified archive kept: %v", entries)
	}
}

func TestPullRefusesNonFileTargets(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.BackupTarget
		want string
	}{
		{config.BackupTarget{Repository: "/srv/restic", PasswordSecret: "PW"}, "restic"},
		{config.BackupTarget{Type: "restic", Repository: "s3:bucket/x"}, "restic"},
		{config.BackupTarget{Type: "archive", Format: "7z", Repository: "gdrive:yoho"}, "rclone"},
	} {
		o := pullOpts("/x", t.TempDir())
		o.Target = tc.cfg
		h := &fakeHost{}
		o.Host = h
		_, err := Pull(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(h.cmds) != 0 {
			t.Errorf("%+v: err %v, cmds %d", tc.cfg, err, len(h.cmds))
		}
	}
}
