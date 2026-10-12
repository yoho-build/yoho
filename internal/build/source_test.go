package build

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// writeTree creates files under dir; a key ending in "/" is an empty dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo is a temp dir with files, as a git work tree when git is true.
func repo(t *testing.T, git bool, files map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := t.TempDir()
	writeTree(t, dir, files)
	if git {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		runGit(t, dir, "init", "-q")
	}
	return dir
}

func buildProject(dir string, b *types.BuildConfig) *types.Project {
	return &types.Project{WorkingDir: dir, Services: types.Services{"w": {Name: "w", WorkloadSpec: types.WorkloadSpec{Build: b}}}}
}

// shipped plans a server build of service w and returns the files (and,
// with a trailing slash, the directories) the exclude list lets through.
func shipped(t *testing.T, o Options) ([]string, *sourcePlan) {
	t.Helper()
	plan, err := planSource(o, []string{"w"})
	if err != nil {
		t.Fatal(err)
	}
	excl := map[string]bool{}
	for _, e := range plan.excludes {
		excl[e] = true
	}
	var got []string
	err = filepath.WalkDir(plan.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relTo(plan.root, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if excl["/"+rsyncEscape(rel)+"/"] {
				return filepath.SkipDir
			}
			got = append(got, rel+"/")
			return nil
		}
		if !excl["/"+rsyncEscape(rel)] {
			got = append(got, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got, plan
}

func filesOnly(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !strings.HasSuffix(p, "/") {
			out = append(out, p)
		}
	}
	return out
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s\n got %q\nwant %q", what, got, want)
	}
}

func TestResolveInputs(t *testing.T) {
	dir := repo(t, false, map[string]string{
		"app/Dockerfile": "", "app/.dockerignore": "",
		"docker/web.Dockerfile": "", "docker/web.Dockerfile.dockerignore": "",
		"shared/.dockerignore": "", "shared/x": "", "VERSION": "1", "npm.txt": "t",
	})
	p := func(rel string) string { return filepath.Join(dir, rel) }
	o := Options{Dir: dir, Project: &types.Project{WorkingDir: dir, Secrets: types.Secrets{
		"npm": {File: "npm.txt"}, "env": {Environment: "E"},
	}}}
	cases := []struct {
		name string
		b    types.BuildConfig
		want inputs
		err  string
	}{
		{name: "context .dockerignore", b: types.BuildConfig{Context: "app"},
			want: inputs{context: p("app"), dockerfile: p("app/Dockerfile"), ignore: p("app/.dockerignore")}},
		{name: "Dockerfile.dockerignore wins", b: types.BuildConfig{Context: "app", Dockerfile: "../docker/web.Dockerfile"},
			want: inputs{context: p("app"), dockerfile: p("docker/web.Dockerfile"), ignore: p("docker/web.Dockerfile.dockerignore")}},
		{name: "inline uses the context's", b: types.BuildConfig{Context: "app", DockerfileInline: "FROM x"},
			want: inputs{context: p("app"), ignore: p("app/.dockerignore")}},
		{name: "no ignore file", b: types.BuildConfig{Context: "shared/..", Dockerfile: "docker/web.Dockerfile"},
			want: inputs{context: dir, dockerfile: p("docker/web.Dockerfile"), ignore: p("docker/web.Dockerfile.dockerignore")}},
		{name: "additional contexts", b: types.BuildConfig{Context: "app", AdditionalContexts: types.Mapping{
			"shared": "shared", "version": p("VERSION"), "base": "docker-image://alpine:3", "web": "https://example.com/x.git", "svc": "service:api"}},
			want: inputs{context: p("app"), dockerfile: p("app/Dockerfile"), ignore: p("app/.dockerignore"),
				dirs: []contextDir{{dir: p("shared"), ignore: p("shared/.dockerignore")}}, files: []string{p("VERSION")}}},
		{name: "oci-layout rejected", b: types.BuildConfig{Context: "app", AdditionalContexts: types.Mapping{"l": "oci-layout:///tmp/l"}},
			err: "oci-layout:// is not supported"},
		{name: "missing additional context", b: types.BuildConfig{Context: "app", AdditionalContexts: types.Mapping{"m": "missing"}},
			err: "additional context m"},
		{name: "file secrets", b: types.BuildConfig{Context: "app", Secrets: []types.ServiceSecretConfig{{Source: "npm"}, {Source: "env"}}},
			want: inputs{context: p("app"), dockerfile: p("app/Dockerfile"), ignore: p("app/.dockerignore"), secrets: []secretFile{{"npm", p("npm.txt")}}}},
		{name: "remote context", b: types.BuildConfig{Context: "https://github.com/x/y.git"}, want: inputs{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := resolveInputs(o, "w", &c.b)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.want.service = "w"
			if fmt.Sprint(in) != fmt.Sprint(c.want) {
				t.Fatalf("\n got %+v\nwant %+v", in, c.want)
			}
		})
	}
}

func TestTransferRoot(t *testing.T) {
	files := map[string]string{"apps/web/yoho.yml": "", "apps/web/Dockerfile": "", "packages/ui/a.ts": ""}
	outside := t.TempDir()
	cases := []struct {
		name    string
		git     bool
		context string
		link    bool // apps/web/ext -> a dir outside the repo, used as context
		want    string
		err     string
	}{
		{name: "narrow", git: true, context: ".", want: "apps/web"},
		{name: "narrow outside git", context: ".", want: "apps/web"},
		{name: "widened", git: true, context: "../..", want: "."},
		{name: "widened to a sibling", git: true, context: "..", want: "apps"},
		{name: "widened without git", context: "../..", err: "git cannot find a work tree"},
		{name: "symlink escape", git: true, context: "ext", link: true, err: "outside the git work tree"},
		{name: "outside git, symlink outside", context: "ext", link: true, err: "git cannot find a work tree"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := repo(t, c.git, files)
			if c.link {
				if err := os.Symlink(outside, filepath.Join(dir, "apps/web/ext")); err != nil {
					t.Fatal(err)
				}
			}
			yoho := filepath.Join(dir, "apps/web")
			o := Options{Dir: yoho, Project: buildProject(yoho, &types.BuildConfig{Context: c.context, Dockerfile: filepath.Join(yoho, "Dockerfile")})}
			ins, err := allInputs(o, []string{"w"})
			if err != nil {
				t.Fatal(err)
			}
			root, _, err := transferRoot(o, ins)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			top, _ := physical(dir)
			if got := relTo(top, root); got != c.want {
				t.Fatalf("root %s, want %s", got, c.want)
			}
		})
	}
}

func TestKeepSetDockerignore(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		b     types.BuildConfig
		want  []string // files that ship
	}{
		{
			name: "star then re-include a package",
			files: map[string]string{".dockerignore": "*\n!packages/ui/**\n", "Dockerfile": "",
				"packages/ui/a.ts": "", "packages/ui/sub/b.ts": "", "packages/api/c.ts": "", "root.txt": ""},
			b:    types.BuildConfig{Context: "."},
			want: []string{".dockerignore", "Dockerfile", "packages/ui/a.ts", "packages/ui/sub/b.ts"},
		},
		{
			// buildx #850: the parent is excluded, a later ! re-includes a child.
			name: "parent excluded, child re-included",
			files: map[string]string{".dockerignore": "dist\n!dist/keep\n", "Dockerfile": "",
				"dist/keep/x": "", "dist/drop/y": "", "dist/z": "", "src/m": ""},
			b:    types.BuildConfig{Context: "."},
			want: []string{".dockerignore", "Dockerfile", "dist/keep/x", "src/m"},
		},
		{
			name: "excluded directory without ! is skipped",
			files: map[string]string{".dockerignore": "node_modules\n**/*.log\n", "Dockerfile": "",
				"node_modules/a/b.js": "", "x.log": "", "deep/y.log": "", "deep/z": ""},
			b:    types.BuildConfig{Context: "."},
			want: []string{".dockerignore", "Dockerfile", "deep/z"},
		},
		{
			name: "Dockerfile.dockerignore beats .dockerignore",
			files: map[string]string{".dockerignore": "a.txt\n", "docker/web.Dockerfile": "", "docker/web.Dockerfile.dockerignore": "b.txt\ndocker\n",
				"a.txt": "", "b.txt": ""},
			b:    types.BuildConfig{Context: ".", Dockerfile: "docker/web.Dockerfile"},
			want: []string{".dockerignore", "a.txt", "docker/web.Dockerfile", "docker/web.Dockerfile.dockerignore"},
		},
		{
			name:  "inline Dockerfile uses only the context's",
			files: map[string]string{".dockerignore": "b\n", "Dockerfile.dockerignore": "a\n", "a": "", "b": ""},
			b:     types.BuildConfig{Context: ".", DockerfileInline: "FROM alpine"},
			want:  []string{".dockerignore", "Dockerfile.dockerignore", "a"},
		},
		{
			name:  "Dockerfile outside the context is kept",
			files: map[string]string{"app/main.go": "", "docker/Dockerfile": "", "docker/other.txt": "", "stray.txt": ""},
			b:     types.BuildConfig{Context: "app", Dockerfile: "../docker/Dockerfile"},
			want:  []string{"app/main.go", "docker/Dockerfile"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := repo(t, false, c.files)
			got, _ := shipped(t, Options{Dir: dir, Project: buildProject(dir, &c.b)})
			eq(t, "shipped", filesOnly(got), c.want)
		})
	}
}

func TestDockerignoreParseErrorNamesLine(t *testing.T) {
	dir := repo(t, false, map[string]string{".dockerignore": "# ok\na\n[\n", "Dockerfile": ""})
	_, err := planSource(Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})}, []string{"w"})
	if err == nil || !strings.Contains(err.Error(), ".dockerignore:3:") {
		t.Fatalf("err %v", err)
	}
}

// monorepo is a bun-workspace-style repo with the Yoho file in apps/web.
var monorepo = map[string]string{
	".gitignore":                      "node_modules/\n/apps/web/.env.local\n*.log\ngen/\n",
	"package.json":                    "{}",
	"bun.lock":                        "",
	"README.md":                       "",
	"my file [1].txt":                 "",
	"pkg/[a] b/x.txt":                 "",
	"apps/shop/.env":                  "SECRET",
	"apps/mac/big.swift":              "",
	"apps/web/yoho.yml":               "",
	"apps/web/Dockerfile":             "",
	"apps/web/src/main.ts":            "",
	"apps/web/.env.local":             "SECRET",
	"apps/web/debug.log":              "",
	"apps/web/gen/api.ts":             "",
	"apps/web/.yoho/secrets":          "SECRET",
	"apps/web/.yoho/hooks/README":     "",
	"packages/ui/index.ts":            "",
	"packages/ui/node_modules/dep.js": "",
}

func TestKeepSetMonorepo(t *testing.T) {
	cases := []struct {
		name      string
		gitignore *bool
		ignore    string // root .dockerignore
		context   string
		want      []string
	}{
		{
			name: "repo root context", context: "../..",
			ignore: "apps/mac\napps/shop\n*.md\nmy file*\npkg\n",
			want: []string{".dockerignore", ".gitignore", "apps/web/.yoho/hooks/README", "apps/web/Dockerfile", "apps/web/src/main.ts",
				"apps/web/yoho.yml", "bun.lock", "package.json", "packages/ui/index.ts"},
		},
		{
			name: "gitignore false still drops .yoho/secrets", context: "../..", gitignore: new(bool),
			ignore: "apps/mac\napps/shop\n*.md\nmy file*\npkg\n",
			want: []string{".dockerignore", ".gitignore", "apps/web/.env.local", "apps/web/.yoho/hooks/README", "apps/web/Dockerfile",
				"apps/web/debug.log", "apps/web/gen/api.ts", "apps/web/src/main.ts", "apps/web/yoho.yml", "bun.lock", "package.json",
				"packages/ui/index.ts", "packages/ui/node_modules/dep.js"},
		},
		{
			// The transfer root is apps/, not the git top level: /apps/web/.env.local
			// and the other patterns must be rebased.
			name: "gitignore rebased onto a narrower root", context: "..",
			want: []string{"web/.yoho/hooks/README", "web/Dockerfile", "web/src/main.ts", "web/yoho.yml", "shop/.env", "mac/big.swift"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range monorepo {
				files[k] = v
			}
			if c.ignore != "" {
				files[".dockerignore"] = c.ignore
			}
			dir := repo(t, true, files)
			yoho := filepath.Join(dir, "apps/web")
			o := Options{Dir: yoho, Builder: config.Builder{Gitignore: c.gitignore},
				Project: buildProject(yoho, &types.BuildConfig{Context: c.context, Dockerfile: "apps/web/Dockerfile"})}
			if c.context == ".." {
				o.Project.Services["w"].Build.Dockerfile = "web/Dockerfile"
			}
			got, plan := shipped(t, o)
			eq(t, "shipped", filesOnly(got), c.want)
			for _, e := range plan.excludes {
				if strings.Contains(e, ".yoho/secrets") {
					return
				}
			}
			t.Errorf(".yoho/secrets must always be excluded: %q", plan.excludes)
		})
	}
}

func TestKeepSetAdditionalContextAndStrays(t *testing.T) {
	files := map[string]string{}
	for k, v := range monorepo {
		files[k] = v
	}
	files["packages/ui/.dockerignore"] = "*.md\n"
	files["packages/ui/notes.md"] = ""
	files["packages/ui/[x] y.ts"] = ""
	files["apps/web/empty/"] = ""
	files["VERSION"] = "1"
	files["gen/token"] = "t" // file secret in a git-ignored dir
	dir := repo(t, true, files)
	yoho := filepath.Join(dir, "apps/web")
	o := Options{Dir: yoho, Project: buildProject(yoho, &types.BuildConfig{Context: ".",
		AdditionalContexts: types.Mapping{"ui": "../../packages/ui", "version": "../../VERSION", "base": "docker-image://alpine"},
		Secrets:            []types.ServiceSecretConfig{{Source: "tok"}}})}
	o.Project.Secrets = types.Secrets{"tok": {File: "../../gen/token"}}
	got, plan := shipped(t, o)
	eq(t, "shipped", got, []string{"VERSION", "apps/", "apps/web/", "apps/web/.yoho/", "apps/web/.yoho/hooks/", "apps/web/.yoho/hooks/README",
		"apps/web/Dockerfile", "apps/web/empty/", "apps/web/src/", "apps/web/src/main.ts", "apps/web/yoho.yml",
		"gen/", "gen/token", "packages/", "packages/ui/", "packages/ui/.dockerignore", "packages/ui/[x] y.ts", "packages/ui/index.ts"})
	for _, want := range []string{`/my file \[1\].txt`, `/pkg/`, `/README.md`, `/apps/shop/`, `/apps/mac/`, `/apps/web/.yoho/secrets`, `/packages/ui/notes.md`} {
		found := false
		for _, e := range plan.excludes {
			found = found || e == want
		}
		if !found {
			t.Errorf("no exclude %q in %q", want, plan.excludes)
		}
	}
	if plan.files != 9 {
		t.Errorf("files %d", plan.files)
	}

	// Server paths are relative to the transfer root (the repo).
	o.root, o.SourceDir = plan.root, "/s"
	script, _, err := ServerScript(o, "w", o.Project.Services["w"], "img")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'--build-context' 'base=docker-image://alpine'", "'--build-context' 'ui=/s/packages/ui'",
		"'--build-context' 'version=/s/VERSION'", "'--secret' 'id=tok,src=/s/gen/token'", "'/s/apps/web'"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %s:\n%s", want, script)
		}
	}
}

func TestEscapedExcludes(t *testing.T) {
	dir := repo(t, false, map[string]string{"Dockerfile": "", ".dockerignore": "my*\npkg\nback*\n",
		"my file [1].txt": "", "pkg/[a] b/x": "", `back\slash`: "", "plain name.txt": ""})
	_, plan := shipped(t, Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})})
	eq(t, "excludes", plan.excludes, []string{`/back\slash`, `/my file \[1\].txt`, `/pkg/`})
}

func TestFileSecretUnderYohoSecretsFails(t *testing.T) {
	dir := repo(t, false, map[string]string{"Dockerfile": "", ".yoho/secrets.d/npm": "t"})
	p := buildProject(dir, &types.BuildConfig{Context: ".", Secrets: []types.ServiceSecretConfig{{Source: "npm"}}})
	p.Secrets = types.Secrets{"npm": {File: ".yoho/secrets.d/npm"}}
	if _, err := planSource(Options{Dir: dir, Project: p}, []string{"w"}); err == nil || !strings.Contains(err.Error(), "never ship") {
		t.Fatalf("err %v", err)
	}
}

func TestCaseCollision(t *testing.T) {
	dir := repo(t, true, map[string]string{"Dockerfile": "", "a.txt": "x"})
	runGit(t, dir, "add", "Dockerfile", "a.txt")
	// A case-insensitive volume cannot hold both names; add the second to
	// the index only.
	blob := runGit(t, dir, "hash-object", "-w", "a.txt")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",A.txt")
	_, err := planSource(Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})}, []string{"w"})
	if err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Fatalf("err %v", err)
	}
	// Not kept: no collision.
	writeTree(t, dir, map[string]string{".dockerignore": "a.txt\nA.txt\n"})
	if _, err := planSource(Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})}, []string{"w"}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceWarnings(t *testing.T) {
	dir := repo(t, true, map[string]string{"apps/web/Dockerfile": "", "apps/web/npm.txt": "t", "apps/web/.dockerignore": "other\n",
		"apps/api/Dockerfile": "", "apps/api/.dockerignore": "npm.txt\n", "apps/api/npm.txt": "t"})
	yoho := filepath.Join(dir, "apps/web")
	p := &types.Project{WorkingDir: yoho, Secrets: types.Secrets{"npm": {File: "npm.txt"}, "api": {File: "../api/npm.txt"}},
		Services: types.Services{
			"web":  {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: ".", Secrets: []types.ServiceSecretConfig{{Source: "npm"}}}}},
			"api":  {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "../api", Secrets: []types.ServiceSecretConfig{{Source: "api"}}}}},
			"root": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "../..", Dockerfile: "apps/web/Dockerfile"}}},
		}}
	ws, err := SourceWarnings(Options{Dir: yoho, Project: p})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.Service+": "+w.Message)
	}
	// The repo-root context of "root" holds both Services' secrets: each is
	// checked against every context, not only its own Service's.
	if len(got) != 4 || !strings.HasPrefix(got[0], "root: server build syncs from") || !strings.Contains(got[0], "has no .dockerignore") ||
		!strings.HasPrefix(got[1], "root: build secret api") || !strings.HasPrefix(got[2], "root: build secret npm") ||
		!strings.HasPrefix(got[3], "web: build secret npm") {
		t.Fatalf("%q", got)
	}
	p.Services["x"] = types.ServiceConfig{WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: ".", AdditionalContexts: types.Mapping{"l": "oci-layout:///x"}}}}
	if _, err := SourceWarnings(Options{Dir: yoho, Project: p}); err == nil {
		t.Fatal("want oci-layout error")
	}
}

// treeOf lists dir: files, "dir/" and "link -> target", with mode bits for
// executables.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relTo(dir, p)
		if rel == "." {
			return nil
		}
		info, _ := d.Info()
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out = append(out, rel+" -> "+target)
		case d.IsDir():
			out = append(out, rel+"/")
		case info.Mode()&0o111 != 0:
			out = append(out, fmt.Sprintf("%s %o", rel, info.Mode().Perm()))
		default:
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestSyncSourceRsync runs the real local rsync twice (the second seeded from
// the first), proving the exclude list ships exactly the keep set.
func TestSyncSourceRsync(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	files := map[string]string{}
	for k, v := range monorepo {
		files[k] = v
	}
	files["packages/ui/.dockerignore"] = "*.md\n"
	files["packages/ui/notes.md"] = ""
	files["packages/ui/[x] y.ts"] = ""
	files["apps/web/empty/"] = ""
	files["apps/web/run.sh"] = "#!/bin/sh\n"
	dir := repo(t, true, files)
	yoho := filepath.Join(dir, "apps/web")
	if err := os.Chmod(filepath.Join(yoho, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(yoho, "link")); err != nil {
		t.Fatal(err)
	}
	o := Options{Dir: yoho, Out: io.Discard, Host: &remote.Local{HostName: "local"},
		Project: buildProject(yoho, &types.BuildConfig{Context: ".", AdditionalContexts: types.Mapping{"ui": "../../packages/ui"}}),
		Exec: func(ctx context.Context, c Command) error {
			out, err := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, out)
			}
			return nil
		}}
	srv := t.TempDir()
	o.cacheDir = filepath.Join(srv, "app", "source")
	sync := func(id string) {
		t.Helper()
		plan, err := planSource(o, []string{"w"})
		if err != nil {
			t.Fatal(err)
		}
		r := o
		r.runDir = o.cacheDir + ".run-" + id
		r.SourceDir = filepath.Join(r.runDir, "source")
		if err := syncSource(context.Background(), r, plan); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(r.runDir)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("private dir %v %v", st.Mode(), err)
		}
		finishSource(r, true)
	}
	sync("a")
	eq(t, "first sync", treeOf(t, o.cacheDir), []string{"apps/", "apps/web/", "apps/web/.yoho/", "apps/web/.yoho/hooks/", "apps/web/.yoho/hooks/README",
		"apps/web/Dockerfile", "apps/web/empty/", "apps/web/link -> run.sh", "apps/web/run.sh 755", "apps/web/src/", "apps/web/src/main.ts",
		"apps/web/yoho.yml", "packages/", "packages/ui/", "packages/ui/.dockerignore", "packages/ui/[x] y.ts", "packages/ui/index.ts"})
	if st, _ := os.Stat(o.cacheDir); st.Mode().Perm() != 0o700 {
		t.Errorf("seed copy mode %o", st.Mode().Perm())
	}

	// Stale files go: one deleted locally, one now dockerignored, one that
	// only exists in the seed.
	os.Remove(filepath.Join(yoho, "src/main.ts"))
	writeTree(t, yoho, map[string]string{".dockerignore": "yoho.yml\n"})
	writeTree(t, o.cacheDir, map[string]string{"stale.txt": "", "apps/mac/old.swift": ""})
	sync("b")
	eq(t, "second sync", treeOf(t, o.cacheDir), []string{"apps/", "apps/web/", "apps/web/.dockerignore", "apps/web/.yoho/", "apps/web/.yoho/hooks/",
		"apps/web/.yoho/hooks/README", "apps/web/Dockerfile", "apps/web/empty/", "apps/web/link -> run.sh", "apps/web/run.sh 755", "apps/web/src/",
		"packages/", "packages/ui/", "packages/ui/.dockerignore", "packages/ui/[x] y.ts", "packages/ui/index.ts"})
	if m, _ := filepath.Glob(o.cacheDir + ".run-*"); len(m) != 0 {
		t.Errorf("private dirs left: %v", m)
	}
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

func chmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o755) })
}

// Finding 1: a symlinked ignore file, Dockerfile or file secret ships by its
// expected name and with every link to its target.
func TestSymlinkedProtectedInputs(t *testing.T) {
	dir := repo(t, false, map[string]string{
		"rules/shared.ignore": "*.tmp\nsecrets\nnotes\n", "rules/other": "",
		"docker/Dockerfile": "FROM alpine\n", "docker/web.Dockerfile": "FROM alpine\n", "docker/web.ignore": "*.tmp\n",
		"real/npm.txt": "t", "app/main.go": "", "app/x.tmp": "", "app/notes": "",
	})
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("../rules/shared.ignore", "app/.dockerignore")
	link("../docker/Dockerfile", "app/Dockerfile")
	link("../docker/web.Dockerfile", "app/web.Dockerfile")
	link("../docker/web.ignore", "app/web.Dockerfile.dockerignore")
	link("../real", "app/secrets")
	link("npm.txt", "real/npm.link")

	p := buildProject(dir, &types.BuildConfig{Context: "app", Secrets: []types.ServiceSecretConfig{{Source: "npm"}}})
	p.Secrets = types.Secrets{"npm": {File: "real/npm.link"}}
	got, plan := shipped(t, Options{Dir: dir, Project: p})
	eq(t, "default Dockerfile", filesOnly(got), []string{"app/.dockerignore", "app/Dockerfile", "app/main.go", "app/web.Dockerfile",
		"app/web.Dockerfile.dockerignore", "docker/Dockerfile", "real/npm.link", "real/npm.txt", "rules/shared.ignore"})
	o := Options{Dir: dir, Project: p, root: plan.root, SourceDir: "/s"}
	script, _, err := ServerScript(o, "w", p.Services["w"], "img")
	if err != nil || !strings.Contains(script, "'id=npm,src=/s/real/npm.link'") {
		t.Fatalf("%v\n%s", err, script)
	}

	// An explicit symlinked Dockerfile keeps the name BuildKit derives the
	// ignore file from: app/web.Dockerfile.dockerignore, not docker/web.ignore's.
	p.Services["w"].Build.Dockerfile = "web.Dockerfile"
	got, plan = shipped(t, Options{Dir: dir, Project: p})
	eq(t, "explicit Dockerfile", filesOnly(got), []string{"app/.dockerignore", "app/Dockerfile", "app/main.go", "app/notes", "app/secrets", "app/web.Dockerfile",
		"app/web.Dockerfile.dockerignore", "docker/web.Dockerfile", "docker/web.ignore", "real/npm.link", "real/npm.txt"})
	o.root = plan.root
	if script, _, err = ServerScript(o, "w", p.Services["w"], "img"); err != nil || !strings.Contains(script, "'--file' '/s/app/web.Dockerfile'") {
		t.Fatalf("%v\n%s", err, script)
	}
}

// Finding 2: with nothing to exclude, a seeded file that builder.exclude
// now matches must still be deleted.
func TestSyncSourceDeletesNewlyExcluded(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	dir := repo(t, false, map[string]string{"Dockerfile": "FROM alpine\n", "token.txt": "t"})
	o := Options{Dir: dir, Out: io.Discard, Host: &remote.Local{HostName: "local"},
		Project: buildProject(dir, &types.BuildConfig{Context: "."}), Exec: realExec}
	o.cacheDir = filepath.Join(t.TempDir(), "app", "source")
	for i, exclude := range [][]string{nil, {"token.txt"}} {
		o.Builder.Exclude = exclude
		plan, err := planSource(o, []string{"w"})
		if err != nil || len(plan.excludes) != 0 {
			t.Fatalf("plan %v %v", plan, err)
		}
		r := o
		r.runDir = fmt.Sprintf("%s.run-%d", o.cacheDir, i)
		r.SourceDir = filepath.Join(r.runDir, "source")
		if err := syncSource(context.Background(), r, plan); err != nil {
			t.Fatal(err)
		}
		finishSource(r, true)
	}
	eq(t, "cache", treeOf(t, o.cacheDir), []string{"Dockerfile"})
}

func realExec(ctx context.Context, c Command) error {
	out, err := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// Finding 3: without a dockerfile setting BuildKit falls back to
// lowercase dockerfile; that file is the one protected.
func TestDefaultDockerfileLowercase(t *testing.T) {
	dir := repo(t, false, map[string]string{"dockerfile": "FROM alpine\n", "dockerfile.dockerignore": "*\n", "a": ""})
	in, err := resolveInputs(Options{Dir: dir, Project: buildProject(dir, nil)}, "w", &types.BuildConfig{Context: "."})
	if err != nil || filepath.Base(in.dockerfile) != "dockerfile" || filepath.Base(in.ignore) != "dockerfile.dockerignore" {
		t.Fatalf("%+v %v", in, err)
	}
	got, _ := shipped(t, Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})})
	eq(t, "shipped", got, []string{"dockerfile", "dockerfile.dockerignore"})
}

// Finding 4: an unreadable directory the ignore file excludes does not fail
// the build; an unreadable directory that ships does.
func TestUnreadableDirectories(t *testing.T) {
	skipIfRoot(t)
	cases := []struct {
		name, ignore string
		err          bool
	}{
		{name: "excluded, no ! below it", ignore: "cache\n!src/keep\n"},
		{name: "excluded, wildcard ! could match below", ignore: "cache\n!**/keep\n"},
		{name: "not excluded", ignore: "other\n", err: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := repo(t, false, map[string]string{"Dockerfile": "", ".dockerignore": c.ignore, "cache/x": "", "src/keep": ""})
			chmod(t, filepath.Join(dir, "cache"), 0)
			_, err := planSource(Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})}, []string{"w"})
			if (err != nil) != c.err {
				t.Fatalf("err %v", err)
			}
		})
	}
	pm, _ := readIgnore(writeIgnore(t, "*\n!packages/ui/**\n"))
	for dir, want := range map[string]bool{"packages": true, "packages/ui": true, "apps": false, "packages/api": false} {
		if got := reincludeBelow(pm, dir); got != want {
			t.Errorf("reincludeBelow(%s) = %v", dir, got)
		}
	}
}

func writeIgnore(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), ".dockerignore")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Finding 5: a Service's file secret inside another Service's additional
// context is reported.
func TestSourceWarningsAcrossServices(t *testing.T) {
	dir := repo(t, false, map[string]string{"web/Dockerfile": "", "api/Dockerfile": "", "shared/.dockerignore": "other\n", "shared/token": "t"})
	p := &types.Project{WorkingDir: dir, Secrets: types.Secrets{"tok": {File: "shared/token"}},
		Services: types.Services{
			"web": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "web", Secrets: []types.ServiceSecretConfig{{Source: "tok"}}}}},
			"api": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "api", AdditionalContexts: types.Mapping{"shared": "shared"}}}},
		}}
	ws, err := SourceWarnings(Options{Dir: dir, Project: p})
	if err != nil || len(ws) != 1 || ws[0].Service != "api" || !strings.Contains(ws[0].Message, "build secret tok") {
		t.Fatalf("%+v %v", ws, err)
	}
}

// Finding 6: promotion never nests the run's source inside an existing
// cache, and a held lock leaves the cache alone.
func TestFinishSourcePromotion(t *testing.T) {
	srv := t.TempDir()
	cache := filepath.Join(srv, "source")
	run := func(id, file string) Options {
		o := Options{Host: &remote.Local{}, Out: io.Discard, cacheDir: cache, runDir: cache + ".run-" + id}
		o.SourceDir = filepath.Join(o.runDir, "source")
		writeTree(t, o.SourceDir, map[string]string{file: ""})
		return o
	}
	writeTree(t, cache, map[string]string{"old": ""})
	finishSource(run("a", "new"), true)
	eq(t, "replaced", treeOf(t, cache), []string{"new"})

	lock := cache + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	o := run("b", "other")
	finishSource(o, true)
	eq(t, "locked", treeOf(t, cache), []string{"new"})
	if _, err := os.Stat(o.runDir); err == nil {
		t.Error("run dir left behind")
	}

	// A lock created just now is live, whatever find's -mtime rounding.
	if err := os.Chtimes(lock, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	finishSource(run("d", "other"), true)
	eq(t, "fresh lock", treeOf(t, cache), []string{"new"})

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	finishSource(run("c", "fresh"), true)
	eq(t, "stale lock", treeOf(t, cache), []string{"fresh"})
	if m, _ := filepath.Glob(filepath.Join(srv, "*")); len(m) != 1 {
		t.Errorf("left behind: %v", m)
	}
}

// Finding 7: a git failure other than "not a repository" skips gitignore
// for a narrow build and fails a widened one, whatever git's language.
func TestBrokenGitConfig(t *testing.T) {
	dir := repo(t, true, map[string]string{".gitignore": ".env.local\n", "apps/web/Dockerfile": "", "apps/web/.env.local": "x", "lib/a": ""})
	bad := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(bad, []byte("[[[ not config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", bad)
	t.Setenv("LANG", "de_DE.UTF-8")
	yoho := filepath.Join(dir, "apps/web")
	got, _ := shipped(t, Options{Dir: yoho, Project: buildProject(yoho, &types.BuildConfig{Context: "."})})
	eq(t, "narrow", got, []string{".env.local", "Dockerfile"})
	_, err := planSource(Options{Dir: yoho, Project: buildProject(yoho, &types.BuildConfig{Context: "../..", Dockerfile: "apps/web/Dockerfile"})}, []string{"w"})
	if err == nil || !strings.Contains(err.Error(), "git cannot find a work tree") {
		t.Fatalf("widened: %v", err)
	}
}

// Finding 8: an ignore file that exists but cannot be read fails the build.
func TestUnreadableIgnoreFile(t *testing.T) {
	skipIfRoot(t)
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		t.Run(name, func(t *testing.T) {
			dir := repo(t, false, map[string]string{"Dockerfile": "", name: "secret\n", "secret": ""})
			chmod(t, filepath.Join(dir, name), 0)
			_, err := planSource(Options{Dir: dir, Project: buildProject(dir, &types.BuildConfig{Context: "."})}, []string{"w"})
			if err == nil || !strings.Contains(err.Error(), "ignore file") || !strings.Contains(err.Error(), name) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

// Finding 9: the private dir check looks at permission bits, not ls output.
func TestPrivateDirCheck(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o700, true}, {0o700 | os.ModeSetgid, true}, {0o700 | os.ModeSticky, true},
		{0o750, false}, {0o701, false}, {0o710, false},
	}
	for _, c := range cases {
		d := filepath.Join(base, fmt.Sprintf("d%o", uint32(c.mode)))
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		chmod(t, d, c.mode)
		err := (&remote.Local{}).Run(context.Background(), remote.Cmd{Script: "set -eu\n" + privateDirCheck(d)})
		if (err == nil) != c.ok {
			t.Errorf("%v: err %v", c.mode, err)
		}
	}
	target := filepath.Join(base, "d700")
	if err := os.Symlink(target, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := (&remote.Local{}).Run(context.Background(), remote.Cmd{Script: privateDirCheck(filepath.Join(base, "link"))}); err == nil {
		t.Error("a symlink must not pass")
	}
}

// Symlinked file inputs ship as links, so a link must resolve on the Server:
// absolute targets and directory symlinks on the way are rejected, relative
// file-to-file chains work.
func TestSymlinkInputsRejected(t *testing.T) {
	cases := []struct {
		name  string
		links map[string]string // link -> target ("@abs/" prefix: absolute, under the repo)
		b     types.BuildConfig
		err   string
		want  []string
	}{
		{
			name:  "through a directory symlink",
			links: map[string]string{"app/via": "../docker", "app/Dockerfile": "via/Dockerfile"},
			b:     types.BuildConfig{Context: "app"},
			err:   "points through a directory symlink",
		},
		{
			// Lexical cleaning would hide via: the real target is real/sub/../token.
			name:  "non-normalized target",
			links: map[string]string{"app/via": "../real/sub", "app/token": "via/../token"},
			b:     types.BuildConfig{Context: "app", Secrets: []types.ServiceSecretConfig{{Source: "tok"}}},
			err:   "non-normalized target",
		},
		{
			name:  "absolute target",
			links: map[string]string{"app/token": "@abs/real/token"},
			b:     types.BuildConfig{Context: "app", Secrets: []types.ServiceSecretConfig{{Source: "tok"}}},
			err:   "absolute target",
		},
		{
			name:  "relative chain",
			links: map[string]string{"app/Dockerfile": "../docker/Dockerfile.link", "docker/Dockerfile.link": "Dockerfile", "app/token": "../real/token"},
			b:     types.BuildConfig{Context: "app", Secrets: []types.ServiceSecretConfig{{Source: "tok"}}},
			want:  []string{"app/.dockerignore", "app/Dockerfile", "app/token", "docker/Dockerfile", "docker/Dockerfile.link", "real/token"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := repo(t, false, map[string]string{"app/.dockerignore": "token\n", "docker/Dockerfile": "FROM alpine\n", "real/token": "t", "docker/x": ""})
			for link, target := range c.links {
				if rest, ok := strings.CutPrefix(target, "@abs/"); ok {
					target = filepath.Join(dir, rest)
				}
				if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
					t.Fatal(err)
				}
			}
			p := buildProject(dir, &c.b)
			p.Secrets = types.Secrets{"tok": {File: "app/token"}}
			o := Options{Dir: dir, Project: p}
			if c.err != "" {
				_, err := planSource(o, []string{"w"})
				if err == nil || !strings.Contains(err.Error(), c.err) || !strings.Contains(err.Error(), "relative symlink") {
					t.Fatalf("err %v", err)
				}
				return
			}
			got, _ := shipped(t, o)
			eq(t, "shipped", filesOnly(got), c.want)
		})
	}
}
