package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const projectYoho = `app: sample
servers:
  prod1: {ssh: root@203.0.113.10}
  w1: {ssh: root@203.0.113.11}
  w2: {ssh: root@203.0.113.12}
destinations:
  production: {servers: [prod1]}
  cluster: {servers: [w1, w2], runtime: swarm}
builder: {location: server}
secrets:
  providers:
    vault: {type: op}
  values:
    DB_PASSWORD: {provider: vault, ref: "op://Prod/db/password-S3CR3T-REF"}
backups:
  targets:
    local: {type: archive, repository: /var/lib/yoho/backup-archives/sample}
  jobs:
    nightly: {destination: production, target: local, schedule: "0 3 * * *"}
    weekly: {destination: cluster, target: local}
`

// projectRepo writes a git repo with the Yoho file in server/ and returns
// the repo root and the Yoho file path.
func projectRepo(t *testing.T, yoho string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{".git", filepath.Join("server", ".yoho")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "server", "yoho.yml")
	if err := os.WriteFile(path, []byte(yoho), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", ".yoho", "secrets"), []byte("API_KEY=plain-file-S3CR3T\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func runProjectSkill(t *testing.T, config string, jsonOut bool, args ...string) (string, error) {
	t.Helper()
	c := skillCmd(&globals{configPath: config, json: jsonOut})
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(append([]string{"install", "--project", "--only", "yoho"}, args...))
	err := c.Execute()
	return buf.String(), err
}

func TestSkillInstallProjectGolden(t *testing.T) {
	root, path := projectRepo(t, projectYoho)
	out, err := runProjectSkill(t, path, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	codex := filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md")
	got, err := os.ReadFile(codex)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "yoho-deploy-sample", "SKILL.md"))
	if err != nil || !bytes.Equal(got, claude) {
		t.Fatalf("claude copy differs: %v", err)
	}
	golden := filepath.Join("testdata", "skill_project.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("SKILL.md differs from %s (go test -run TestSkillInstallProjectGolden -update):\n%s", golden, got)
	}
	if strings.Contains(string(got), "S3CR3T") {
		t.Error("secret ref or value leaked into the generated skill")
	}
	if strings.Contains(out, "S3CR3T") {
		t.Error("secret leaked into output")
	}

	// A rerun is byte-identical.
	if out, err := runProjectSkill(t, path, false); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	again, _ := os.ReadFile(codex)
	if !bytes.Equal(got, again) {
		t.Error("rerun changed the file")
	}
}

func TestSkillInstallProjectRuntimes(t *testing.T) {
	compose := strings.Replace(projectYoho, "  cluster: {servers: [w1, w2], runtime: swarm}\n", "", 1)
	compose = strings.Replace(compose, "    weekly: {destination: cluster, target: local}\n", "", 1)
	compose = strings.Replace(compose, "location: server", "location: local", 1)
	root, path := projectRepo(t, compose)
	if out, err := runProjectSkill(t, path, false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md"))
	s := string(b)
	for _, want := range []string{"runtime `compose`", "yoho deploy -d production", "Docker must be running", "yoho backup run nightly"} {
		if !strings.Contains(s, want) {
			t.Errorf("compose skill lacks %q", want)
		}
	}
	for _, bad := range []string{"swarm", "cluster", "weekly"} {
		if strings.Contains(s, bad) {
			t.Errorf("compose skill mentions %q", bad)
		}
	}

	root, path = projectRepo(t, projectYoho)
	if out, err := runProjectSkill(t, path, false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, _ = os.ReadFile(filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md"))
	s = string(b)
	for _, want := range []string{"runtime `swarm`", "Swarm manager", "yoho swarm status -d cluster", "yoho backup run weekly"} {
		if !strings.Contains(s, want) {
			t.Errorf("swarm skill lacks %q", want)
		}
	}
}

func TestSkillInstallProjectRefusesEditsWithoutForce(t *testing.T) {
	root, path := projectRepo(t, projectYoho)
	if out, err := runProjectSkill(t, path, false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	skill := filepath.Join(root, ".claude", "skills", "yoho-deploy-sample", "SKILL.md")
	f, err := os.OpenFile(skill, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\nMY-LOCAL-NOTE\n")
	f.Close()

	_, err = runProjectSkill(t, path, false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want refusal hinting --force, got %v", err)
	}
	if b, _ := os.ReadFile(skill); !strings.Contains(string(b), "MY-LOCAL-NOTE") {
		t.Error("edited file was overwritten without --force")
	}
	if out, err := runProjectSkill(t, path, false, "--force"); err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(skill); strings.Contains(string(b), "MY-LOCAL-NOTE") {
		t.Error("--force did not overwrite")
	}

	// A hand-written skill with the same name (no hash) is also refused.
	if err := os.WriteFile(skill, []byte("---\nname: yoho-deploy-sample\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runProjectSkill(t, path, false); err == nil {
		t.Error("hand-written skill overwritten without --force")
	}
}

func TestSkillInstallProjectSymlinkedLayout(t *testing.T) {
	root, path := projectRepo(t, projectYoho)
	if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectSkill(t, path, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(out, `"project":true`); n != 2 {
		t.Errorf("want 2 project events, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, `"event":"skill_installed","files":1,"path":"`+filepath.Join(root, ".claude", "skills", "yoho-deploy-sample")); n != 1 {
		t.Errorf("want the generated skill written once, under the first agent dir:\n%s", out)
	}
	if n := strings.Count(out, `"event":"skill_linked"`); n != 2 {
		t.Errorf("want 2 skill_linked (yoho and yoho-deploy-sample), got %d:\n%s", n, out)
	}
	if !exists(filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md")) {
		t.Error("generated skill missing in the real folder")
	}
}

func TestSkillInstallProjectDirDefaultsToGitRoot(t *testing.T) {
	root, path := projectRepo(t, projectYoho)
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if out, err := runProjectSkill(t, "", false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md")) {
		t.Error("skill not written at the git root")
	}
	if exists(filepath.Join(root, "server", ".agents")) {
		t.Error("skill written next to the Yoho file")
	}
}

func installedProjectSkill(t *testing.T, root, path string) string {
	t.Helper()
	if out, err := runProjectSkill(t, path, false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	} else if strings.Contains(out, "S3CR3T") {
		t.Fatal("secret leaked into install output")
	}
	b, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "yoho-deploy-sample", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "S3CR3T") {
		t.Fatal("secret value or ref leaked into skill")
	}
	return string(b)
}

func TestSkillInstallProjectMergedDestinations(t *testing.T) {
	base := strings.ReplaceAll(projectYoho, "production", "qa")
	base = strings.ReplaceAll(base, "cluster", "staging")
	base = strings.Replace(base, "prod1: {ssh: root@203.0.113.10}", "prod1: {}", 1)
	root, path := projectRepo(t, base)
	dir := filepath.Dir(path)
	// Each merged config must be valid, even when its base is incomplete.
	for name, overlay := range map[string]string{
		"qa": `servers:
  prod1: {ssh: qa@203.0.113.10}
builder: {location: local}
secrets:
  providers:
    extra: {type: bw, account: account-S3CR3T}
  values:
    API_KEY: {provider: extra, ref: item-S3CR3T}
backups:
  jobs:
    hourly: {destination: qa, target: local}
    weekly: {destination: qa, target: local, schedule: hourly}
    ignored: {destination: staging, target: local}
`,
		"staging": `servers:
  prod1: {ssh: staging@203.0.113.10}
builder: {location: remote, remote: builder@203.0.113.20}
secrets:
  providers:
    extra: {type: bws}
  values:
    API_KEY: {provider: extra, ref: other-S3CR3T}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "yoho."+name+".yml"), []byte(overlay), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := installedProjectSkill(t, root, path)
	for _, want := range []string{
		"### qa\n\nBuilder: `local`", "### staging\n\nBuilder: `remote`",
		"`extra` (bw), `extra` (bws), `vault` (op)", "`API_KEY`, `DB_PASSWORD`",
		"yoho config check -d qa", "yoho config check -d staging",
		"yoho backup run hourly -d qa", "yoho backup list hourly -d qa",
		"yoho backup list nightly -d qa", "yoho backup list weekly -d qa",
		"yoho backup list weekly -d staging", "schedule `hourly`",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("merged skill lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"yoho config check\n", "yoho backup list\n", "ignored", "Builder: `server`"} {
		if strings.Contains(s, bad) {
			t.Errorf("merged skill contains %q", bad)
		}
	}
	for _, once := range []string{"`vault` (op)", "`API_KEY`", "`DB_PASSWORD`", "yoho backup run nightly -d qa"} {
		if n := strings.Count(s, once); n != 1 {
			t.Errorf("%q appears %d times, want 1", once, n)
		}
	}
	// Validation still rejects an invalid merged config.
	if err := os.WriteFile(filepath.Join(dir, "yoho.qa.yml"), []byte("builder: {location: invalid}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectSkill(path); err == nil || !strings.Contains(err.Error(), "invalid yoho.yml for Destination qa") {
		t.Fatalf("want merged validation error, got %v", err)
	}
}

func TestSkillInstallProjectCommandDirectories(t *testing.T) {
	for _, runDir := range []string{"deploy files", "."} {
		t.Run(runDir, func(t *testing.T) {
			root, path := projectRepo(t, projectYoho)
			dir := filepath.Join(root, runDir)
			if runDir != "." {
				if err := os.Rename(filepath.Dir(path), dir); err != nil {
					t.Fatal(err)
				}
			}
			if runDir == "." {
				if err := os.WriteFile(filepath.Join(dir, "yoho.yml"), []byte(projectYoho), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := installedProjectSkill(t, root, filepath.Join(dir, "yoho.yml"))
			blocks := strings.Split(s, "```bash\n")[1:]
			if len(blocks) != 5 {
				t.Fatalf("want 5 command blocks, got %d", len(blocks))
			}
			for _, block := range blocks {
				if runDir != "." && !strings.HasPrefix(block, "cd 'deploy files'\n") {
					t.Errorf("command block lacks quoted cd:\n%s", block)
				}
				if runDir == "." && !strings.HasPrefix(block, "yoho ") {
					t.Errorf("root command block should start with yoho:\n%s", block)
				}
			}
		})
	}
}

func TestSkillInstallProjectExplicitConfig(t *testing.T) {
	for _, name := range []string{"yoho.yaml", "custom config's.yml"} {
		t.Run(name, func(t *testing.T) {
			root, original := projectRepo(t, projectYoho)
			path := filepath.Join(filepath.Dir(original), name)
			if err := os.WriteFile(path, []byte(projectYoho), 0o644); err != nil {
				t.Fatal(err)
			}
			s := installedProjectSkill(t, root, path)
			flag := "-c yoho.yaml "
			if name != "yoho.yaml" {
				flag = "-c 'custom config'\"'\"'s.yml' "
			}
			for _, line := range strings.Split(s, "\n") {
				if strings.HasPrefix(line, "yoho ") && !strings.HasPrefix(line, "yoho "+flag) {
					t.Errorf("command lost explicit config: %s", line)
				}
			}
		})
	}
}

func TestProjectSkillShellQuoting(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"safe_@%+=:,./-123", "safe_@%+=:,./-123"},
		{"", "''"},
		{"deploy files", "'deploy files'"},
		{"it's", "'it'\"'\"'s'"},
		{"qa; $(touch nope)", "'qa; $(touch nope)'"},
		{"line\nbreak", "'line\nbreak'"},
	} {
		if got := projectShellArg(tc.arg); got != tc.want {
			t.Errorf("quote %q = %q, want %q", tc.arg, got, tc.want)
		}
	}
	base := strings.ReplaceAll(projectYoho, "production", "qa; $HOME")
	base = strings.ReplaceAll(base, "nightly", "nightly's backup")
	root, path := projectRepo(t, base)
	s := installedProjectSkill(t, root, path)
	for _, command := range []string{"config check", "plan", "deploy", "app ps", "app logs SERVICE -n 200", "releases", "rollback VERSION", "backup run 'nightly'\"'\"'s backup'", "backup list 'nightly'\"'\"'s backup'"} {
		if want := "yoho " + command + " -d 'qa; $HOME'"; !strings.Contains(s, want) {
			t.Errorf("missing quoted command %q", want)
		}
	}
}
