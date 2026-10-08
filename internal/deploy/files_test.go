package deploy

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

const filesCompose = `
services:
  db:
    image: clickhouse:24
    volumes:
      - ./cfg/init.sql:/docker-entrypoint-initdb.d/init.sql:ro
      - ./conf:/etc/app:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/var/lib/data
    configs: [cors]
  web:
    image: shop-web:v1
configs:
  cors:
    file: ./cors.json
volumes:
  data:
`

// appDirProject writes an App directory with files and loads filesCompose there.
func appDirProject(t *testing.T, src string) (string, *types.Project) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("cfg/init.sql", "create table t;\n", 0o640)
	write("conf/app.conf", "a=1\n", 0o644)
	write("conf/sub/run.sh", "#!/bin/sh\n", 0o755)
	write("conf/.yoho/secrets", "TOKEN=x\n", 0o600)
	write("cors.json", "{}\n", 0o644)
	return dir, reload(t, dir, src)
}

func reload(t *testing.T, dir, src string) *types.Project {
	t.Helper()
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(src)}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) { o.SetProjectName("shop", true) })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func filesDeploy(h remote.Host, p *types.Project, version string) *plan.Deploy {
	return &plan.Deploy{
		App: "shop", Destination: "production", Version: version, Performer: "alice",
		Servers:        []plan.NamedHost{{Name: "primary", Server: config.Server{SSH: "yoho@203.0.113.10"}, Host: h}},
		Project:        p,
		Ext:            map[string]config.ServiceExt{"db": {Stateful: true}},
		RetainReleases: 2,
	}
}

type filesDoc struct {
	Services map[string]struct {
		Labels  map[string]string `yaml:"labels"`
		Volumes []struct {
			Type   string `yaml:"type"`
			Source string `yaml:"source"`
		} `yaml:"volumes"`
	} `yaml:"services"`
	Configs map[string]struct {
		File string `yaml:"file"`
	} `yaml:"configs"`
}

func compileFiles(t *testing.T, d *plan.Deploy) filesDoc {
	t.Helper()
	out, _, err := compile(d, "primary", "/gen", nil, []byte("0123456789abcdef0123456789abcdef"), "")
	if err != nil {
		t.Fatal(err)
	}
	var doc filesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestCompileRewritesAppFiles(t *testing.T) {
	root := withRoot(t)
	dir, p := appDirProject(t, filesCompose)
	doc := compileFiles(t, filesDeploy(nil, p, "v1"))
	filesRoot := path.Join(root, "apps", "shop", "production", "files") + "/"
	db := doc.Services["db"]
	var srcs []string
	for _, v := range db.Volumes {
		srcs = append(srcs, v.Source)
	}
	if len(srcs) != 4 ||
		!strings.HasPrefix(srcs[0], filesRoot) || path.Base(srcs[0]) != "init.sql" ||
		!strings.HasPrefix(srcs[1], filesRoot) || path.Base(srcs[1]) != "conf" ||
		srcs[2] != "/var/run/docker.sock" || srcs[3] != "data" {
		t.Fatalf("volume sources %v", srcs)
	}
	if strings.Contains(strings.Join(srcs, " "), dir) {
		t.Errorf("local App path leaked into compiled compose: %v", srcs)
	}
	if c := doc.Configs["cors"].File; !strings.HasPrefix(c, filesRoot) || path.Base(c) != "cors.json" {
		t.Errorf("config file %q", c)
	}
	label := db.Labels[LabelFiles]
	if len(label) != 16 || doc.Services["web"].Labels[LabelFiles] != "" {
		t.Errorf("files labels db=%q web=%q", label, doc.Services["web"].Labels[LabelFiles])
	}

	// Same content, new Version: same paths (no recreate, no plan change).
	if again := compileFiles(t, filesDeploy(nil, reload(t, dir, filesCompose), "v2")); again.Services["db"].Volumes[0].Source != srcs[0] || again.Services["db"].Labels[LabelFiles] != label {
		t.Error("unchanged App files must keep their Server path across Versions")
	}
	before, _, _ := compile(filesDeploy(nil, p, "v1"), "primary", planGenDir, nil, planKey, "")
	// Mode changes count as content.
	if err := os.Chmod(filepath.Join(dir, "cfg", "init.sql"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := compileFiles(t, filesDeploy(nil, reload(t, dir, filesCompose), "v2"))
	if changed.Services["db"].Volumes[0].Source == srcs[0] || changed.Services["db"].Volumes[1].Source != srcs[1] || changed.Services["db"].Labels[LabelFiles] == label {
		t.Errorf("mode change: %+v", changed.Services["db"])
	}
	// What `yoho plan` compares.
	after, _, _ := compile(filesDeploy(nil, reload(t, dir, filesCompose), "v2"), "primary", planGenDir, nil, planKey, "")
	a, _ := serviceEntries(before)
	b, _ := serviceEntries(after)
	if keys := strings.Join(changedKeys(a["db"], b["db"]), ","); keys != "labels,volumes" {
		t.Errorf("plan sees db changes %q", keys)
	}
	if keys := changedKeys(a["web"], b["web"]); len(keys) != 0 {
		t.Errorf("plan sees web changes %v", keys)
	}
	// The project the caller holds is untouched.
	if p.Services["db"].Volumes[0].Source != filepath.Join(dir, "cfg", "init.sql") {
		t.Error("compile modified d.Project")
	}
}

func TestCompileRejectsWritableAppBind(t *testing.T) {
	withRoot(t)
	_, p := appDirProject(t, "services:\n  db:\n    image: x\n    volumes: [\"./cfg:/cfg\"]\n")
	_, _, err := compile(filesDeploy(nil, p, "v1"), "primary", "/gen", nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), "writable bind mount") || !strings.Contains(err.Error(), "named volume") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppFilesSizeLimit(t *testing.T) {
	withRoot(t)
	dir, _ := appDirProject(t, filesCompose)
	if err := os.Truncate(filepath.Join(dir, "conf", "app.conf"), maxAppFilesBytes+1); err != nil {
		t.Fatal(err)
	}
	_, _, err := compile(filesDeploy(nil, reload(t, dir, filesCompose), "v1"), "primary", "/gen", nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), "50 MB") || !strings.Contains(err.Error(), "named volume") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployShipsAppFilesRollbackAndPrune(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = oldNow })

	dir, p := appDirProject(t, filesCompose)
	appDir := filepath.Join(root, "apps", "shop", "production")
	initSQL := func(version string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(appDir, "releases", version, "compose.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var doc filesDoc
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Services["db"].Volumes[0].Source
	}

	var out bytes.Buffer
	d := filesDeploy(h, p, "v1")
	d.Out = &out
	rel, err := Compose{}.Deploy(ctx, d)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rel.Files) != 3 || !strings.Contains(out.String(), "shipping 3 App file(s)") {
		t.Errorf("files %v\n%s", rel.Files, out.String())
	}
	v1 := initSQL("v1")
	if b, err := os.ReadFile(v1); err != nil || string(b) != "create table t;\n" {
		t.Fatalf("shipped init.sql %q %v", b, err)
	}
	if st, _ := os.Stat(v1); st.Mode().Perm() != 0o640 {
		t.Errorf("init.sql mode %v", st.Mode())
	}
	confDir := filepath.Join(appDir, "files")
	matches, _ := filepath.Glob(filepath.Join(confDir, "*", "conf", "sub", "run.sh"))
	if len(matches) != 1 {
		t.Fatalf("conf tree not shipped: %v", matches)
	}
	if st, _ := os.Stat(matches[0]); st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode %v", st.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(confDir, "*", "conf", ".yoho")); len(m) != 0 {
		t.Errorf(".yoho must never be shipped: %v", m)
	}

	// Unchanged files are not uploaded again.
	out.Reset()
	d = filesDeploy(h, reload(t, dir, filesCompose), "v2")
	d.Out = &out
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "shipping") || initSQL("v2") != v1 {
		t.Errorf("v2 re-shipped unchanged files:\n%s", out.String())
	}

	// Changed content: new path, old one kept for rollback.
	if err := os.WriteFile(filepath.Join(dir, "cfg", "init.sql"), []byte("create table t2;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Compose{}).Deploy(ctx, filesDeploy(h, reload(t, dir, filesCompose), "v3")); err != nil {
		t.Fatal(err)
	}
	v3 := initSQL("v3")
	if b, _ := os.ReadFile(v3); v3 == v1 || string(b) != "create table t2;\n" {
		t.Fatalf("v3 init.sql %s %q", v3, b)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatal("v2's files must be kept while v2 is retained")
	}
	if _, err := (Compose{}).Rollback(ctx, filesDeploy(h, p, ""), "v2"); err != nil {
		t.Fatal(err)
	}

	// v4 and v5 with the new content: v1/v2 are pruned (retain 2) and the old
	// init.sql with them.
	for _, v := range []string{"v4", "v5"} {
		if _, err := (Compose{}).Deploy(ctx, filesDeploy(h, reload(t, dir, filesCompose), v)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Dir(v1)); !os.IsNotExist(err) {
		t.Errorf("old App files should be pruned: %v", err)
	}
	if _, err := os.Stat(v3); err != nil {
		t.Errorf("current App files must stay: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(confDir, ".upload.*")); len(m) != 0 {
		t.Errorf("upload temp dirs left: %v", m)
	}
}
