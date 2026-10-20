package gate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMembers(t *testing.T) {
	m, err := ParseMembers([]byte("members:\n  alice: {verbs: [deploy, logs]}\n  ci: {verbs: [releases]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		member, verb string
		ok           bool
	}{
		{"alice", "deploy", true}, {"alice", "images", true}, {"alice", "logs", true}, {"alice", "hello", true},
		{"alice", "rollback", false}, {"alice", "exec", false},
		{"ci", "hello", true}, {"ci", "images", false}, {"bob", "hello", false},
	} {
		if got := m.Allowed(c.member, c.verb); got != c.ok {
			t.Errorf("%s %s: got %v", c.member, c.verb, got)
		}
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseMembers(b)
	if err != nil || len(back.Members) != 2 || strings.Join(back.Members["alice"].Verbs, ",") != "deploy,logs" {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	for _, bad := range []string{"members: {Alice: {verbs: [deploy]}}", "members: {a: {verbs: [exec]}}", "members: {a: {verbs: [access]}}"} {
		if _, err := ParseMembers([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestParseVerbs(t *testing.T) {
	v, err := ParseVerbs("read,deploy")
	if err != nil || strings.Join(v, ",") != "dash,deploy,logs,ps,releases,stats" {
		t.Fatalf("%v %v", v, err)
	}
	if v, _ := ParseVerbs("all"); len(v) != len(GrantableVerbs) {
		t.Fatalf("all: %v", v)
	}
	for _, bad := range []string{"", "exec", "hello"} {
		if _, err := ParseVerbs(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestValidAppDest(t *testing.T) {
	for _, bad := range [][2]string{{"../x", "p"}, {"a", "../p"}, {"a", "p/q"}, {"", "p"}, {"A", "p"}, {"a", ".."}} {
		if ValidAppDest(bad[0], bad[1]) == nil {
			t.Errorf("%v: want error", bad)
		}
	}
	if err := ValidAppDest("shop", "production"); err != nil {
		t.Fatal(err)
	}
}

func TestAppendAudit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "access", "audit.log")
	for i := range 2 {
		if err := AppendAudit(p, AuditEntry{Time: time.Unix(int64(i), 0), Member: "alice", Verb: "logs", Exit: i}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e AuditEntry
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &e) != nil || e.Exit != 1 || e.Member != "alice" {
		t.Fatalf("audit: %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func archive(t *testing.T, gz bool, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w = &buf
	var zw *gzip.Writer
	tw := tar.NewWriter(w)
	if gz {
		zw = gzip.NewWriter(w)
		tw = tar.NewWriter(zw)
	}
	for _, name := range sortedKeys(files) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[name]))
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

func TestArchiveTags(t *testing.T) {
	a := archive(t, true, map[string]string{
		"blobs/sha256/aa": "layer",
		"manifest.json":   `[{"Config":"blobs/sha256/cc","RepoTags":["yoho/shop-production-web:abc"]}]`,
		"index.json":      `{"manifests":[{"annotations":{"io.containerd.image.name":"docker.io/yoho/shop-production-web:abc","org.opencontainers.image.ref.name":"abc"}}]}`,
		"repositories":    `{"basecamp/kamal-proxy":{"v0.10.2":"cc"}}`,
	})
	tags, err := ArchiveTags(bytes.NewReader(a), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags, ","); got != "basecamp/kamal-proxy:v0.10.2,yoho/shop-production-web:abc" {
		t.Fatalf("tags %q", got)
	}
	if _, err := ArchiveTags(bytes.NewReader(archive(t, false, map[string]string{"x": "y"})), 0); err == nil {
		t.Fatal("want error for an archive without metadata")
	}
}

// TestArchiveTagsRefusesTricks covers the QA archive attacks: names that
// resolve to the metadata files some other way, links, duplicates and bare
// OCI tags.
func TestArchiveTagsRefusesTricks(t *testing.T) {
	ok := `[{"RepoTags":["yoho/shop-production-web:abc"]}]`
	for name, entries := range map[string][]tarEntry{
		"dotdot":          {{name: "/x/../repositories", body: `{"evil":{"latest":"cc"}}`}, {name: "manifest.json", body: ok}},
		"absolute":        {{name: "/manifest.json", body: ok}},
		"dot segment":     {{name: "./manifest.json", body: ok}},
		"double slash":    {{name: "a//manifest.json", body: ok}},
		"duplicate":       {{name: "manifest.json", body: ok}, {name: "manifest.json", body: `[{"RepoTags":["evil:1"]}]`}},
		"symlink dir":     {{name: "alias", link: ".", typ: tar.TypeSymlink}, {name: "alias/manifest.json", body: `[{"RepoTags":["evil:1"]}]`}, {name: "manifest.json", body: ok}},
		"symlink file":    {{name: "manifest.json", link: "blobs/m", typ: tar.TypeSymlink}},
		"hardlink":        {{name: "blobs/m", body: `[{"RepoTags":["evil:1"]}]`}, {name: "manifest.json", link: "blobs/m", typ: tar.TypeLink}},
		"fifo":            {{name: "p", typ: tar.TypeFifo}, {name: "manifest.json", body: ok}},
		"bare oci tag":    {{name: "index.json", body: `{"manifests":[{"annotations":{"org.opencontainers.image.ref.name":"latest"}}]}`}},
		"dir and file":    {{name: "blobs", typ: tar.TypeDir}, {name: "blobs/", typ: tar.TypeDir}, {name: "manifest.json", body: ok}},
		"backslash":       {{name: `x\manifest.json`, body: ok}, {name: "manifest.json", body: ok}},
		"duplicate index": {{name: "index.json", body: `{}`}, {name: "index.json", body: `{}`}},
	} {
		t.Run(name, func(t *testing.T) {
			if tags, err := ArchiveTags(bytes.NewReader(tarOf(t, entries)), 0); err == nil {
				t.Fatalf("want refusal, got tags %v", tags)
			}
		})
	}
}

func TestArchiveTagsLimit(t *testing.T) {
	a := archive(t, true, map[string]string{"blobs/big": strings.Repeat("x", 1<<20), "manifest.json": `[]`})
	if _, err := ArchiveTags(bytes.NewReader(a), 64<<10); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("got %v", err)
	}
	if _, err := ArchiveTags(bytes.NewReader(a), 4<<20); err != nil {
		t.Fatal(err)
	}
}

type tarEntry struct {
	name, body, link string
	typ              byte
}

func tarOf(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return buf.Bytes()
}

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io/library/postgres:17":      "postgres:17",
		"postgres":                           "postgres:latest",
		"ghcr.io/me/app-web:V1":              "ghcr.io/me/app-web:V1",
		"library/yoho/x:1":                   "library/yoho/x:1",
		"GHCR.IO/acme/x:V1":                  "ghcr.io/acme/x:V1",
		"Localhost:5000/x":                   "localhost:5000/x:latest",
		"registry-1.docker.io/library/redis": "redis:latest",
		"docker.io/yoho/x:1":                 "yoho/x:1",
		"localhost:5000/x":                   "localhost:5000/x:latest",
		"x@sha256:abc":                       "x@sha256:abc",
	} {
		if got := NormalizeRef(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
