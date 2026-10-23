package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/secrets"
)

// projectSkill is the generated yoho-deploy-<app> skill. Its data comes only
// from the Yoho file: names, kinds and paths, never secret values or refs.
type projectSkill struct {
	App          string
	ConfigRel    string // Yoho file, relative to the git root
	RunDir       string // directory of the Yoho file, relative to the git root
	ConfigFlag   string // "-c NAME " unless config discovery selects this file
	Destinations []projectDest
	SecretFiles  []string
	Providers    []projectProvider
	SecretKeys   []string
	Jobs         []projectJob
}

type projectDest struct {
	Name, Runtime, Builder string
	Servers                []projectServer
}

type projectServer struct{ Name, SSH string }

type projectProvider struct{ Name, Type string }

type projectJob struct{ Name, Destination, Target, Schedule string }

func (p projectSkill) Name() string { return "yoho-deploy-" + p.App }

func (p projectSkill) DestNames() string {
	var n []string
	for _, d := range p.Destinations {
		n = append(n, d.Name)
	}
	return strings.Join(n, ", ")
}

func (p projectSkill) HasSwarm() bool {
	return slices.ContainsFunc(p.Destinations, func(d projectDest) bool { return d.Runtime == "swarm" })
}

// loadProjectSkill reads the Yoho file and every Destination (with its
// overlay) into the template data.
func loadProjectSkill(path string) (projectSkill, error) {
	base, err := config.Load(path)
	if err != nil {
		return projectSkill{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return projectSkill{}, err
	}
	dir := filepath.Dir(abs)
	root := gitRoot(dir)
	rel := func(p string) string {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(r)
	}
	p := projectSkill{
		App:       base.App,
		ConfigRel: rel(abs),
		RunDir:    rel(dir),
	}
	if found, err := config.Find(dir); err != nil || found != abs {
		p.ConfigFlag = "-c " + projectShellArg(filepath.Base(abs)) + " "
	}
	providers := make(map[projectProvider]bool)
	keys := make(map[string]bool)
	jobs := make(map[[2]string]projectJob)
	names := sortedKeys(base.Destinations)
	if len(names) == 0 {
		_, err := base.DestName("")
		return projectSkill{}, err
	}
	if def, err := base.DestName(""); err == nil {
		names = slices.Insert(slices.DeleteFunc(names, func(n string) bool { return n == def }), 0, def)
	}
	for _, name := range names {
		cfg, _, _, err := config.LoadForDestination(abs, name)
		if err != nil {
			return projectSkill{}, err
		}
		if errs := config.Validate(cfg); len(errs) > 0 {
			return projectSkill{}, fmt.Errorf("invalid %s for Destination %s:\n  %w", filepath.Base(path), name, errors.Join(errs...))
		}
		d := cfg.Destinations[name]
		pd := projectDest{Name: name, Runtime: d.Runtime, Builder: cfg.Builder.Location}
		for _, s := range d.Servers {
			pd.Servers = append(pd.Servers, projectServer{s, cfg.Servers[s].SSH})
		}
		p.Destinations = append(p.Destinations, pd)
		for n, provider := range cfg.Secrets.Providers {
			providers[projectProvider{n, provider.Type}] = true
		}
		for n := range cfg.Secrets.Values {
			keys[n] = true
		}
		for n, j := range cfg.Backups.Jobs {
			if j.Destination == name {
				jobs[[2]string{n, name}] = projectJob{n, name, j.Target, j.Schedule}
			}
		}
		for _, f := range secrets.FileNames(name) {
			full := filepath.Join(dir, f)
			if _, err := os.Stat(full); err == nil && !slices.Contains(p.SecretFiles, rel(full)) {
				p.SecretFiles = append(p.SecretFiles, rel(full))
			}
		}
	}
	slices.Sort(p.SecretFiles)
	for provider := range providers {
		p.Providers = append(p.Providers, provider)
	}
	slices.SortFunc(p.Providers, func(a, b projectProvider) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.Type, b.Type)
	})
	p.SecretKeys = sortedKeys(keys)
	for _, job := range jobs {
		p.Jobs = append(p.Jobs, job)
	}
	slices.SortFunc(p.Jobs, func(a, b projectJob) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.Destination, b.Destination)
	})
	return p, nil
}

// gitRoot is the nearest ancestor of dir holding .git (a folder, or a file in
// a worktree); dir itself when there is none.
func gitRoot(dir string) string {
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		if filepath.Dir(cur) == cur {
			return dir
		}
	}
}

var projectShellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func projectShellArg(arg string) string {
	if projectShellSafeRE.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

var projectSkillTmpl = template.Must(template.New("skill").Funcs(template.FuncMap{"sh": projectShellArg}).Parse(`---
name: {{.Name}}
description: Deploy, check, roll back or back up {{.App}} with Yoho (Destinations {{.DestNames}}). Use for any deploy, release, rollback, backup or log check of {{.App}}.
---

# Deploy {{.App}} with Yoho

Generated from ` + "`{{.ConfigRel}}`" + ` by ` + "`yoho skill install --project`" + `. Rerun that after changing the Yoho file. A rerun refuses to overwrite hand edits unless given ` + "`--force`" + `. The ` + "`yoho`" + ` skill has the general reference.

## Where to run

Run every command from ` + "`{{.RunDir}}`" + ` (relative to the repository root), after committing: an uncommitted tree makes the version end in ` + "`_uncommitted_`" + `.

## Destinations
{{range .Destinations}}
- ` + "`{{.Name}}`" + `: runtime ` + "`{{.Runtime}}`" + `, Servers {{range $i, $s := .Servers}}{{if $i}}, {{end}}` + "`{{$s.Name}}`" + ` (` + "`{{$s.SSH}}`" + `){{end}}{{if eq .Runtime "swarm"}}; the first Server is the Swarm manager{{end}}
{{- end}}

## Deploy
{{range .Destinations}}
### {{.Name}}

Builder: ` + "`{{.Builder}}`" + `. {{if eq .Builder "server"}}Images build on the Server from git-visible files; no local Docker needed.{{else if eq .Builder "remote"}}Images build on the configured remote builder.{{else}}Images build on this machine; Docker must be running.{{end}}

` + "```bash" + `
{{- if ne $.RunDir "."}}
cd {{sh $.RunDir}}
{{- end}}
yoho {{$.ConfigFlag}}config check -d {{sh .Name}}
yoho {{$.ConfigFlag}}plan -d {{sh .Name}}
yoho {{$.ConfigFlag}}deploy -d {{sh .Name}}
` + "```" + `
{{end}}

Read the plan before deploying.

## Check

` + "```bash" + `
{{- if ne .RunDir "."}}
cd {{sh .RunDir}}
{{- end}}
{{- range .Destinations}}
yoho {{$.ConfigFlag}}app ps -d {{sh .Name}}
yoho {{$.ConfigFlag}}app logs SERVICE -n 200 -d {{sh .Name}}
yoho {{$.ConfigFlag}}releases -d {{sh .Name}}
{{- if eq .Runtime "swarm"}}
yoho {{$.ConfigFlag}}swarm status -d {{sh .Name}}
{{- end}}
{{- end}}
` + "```" + `

Never use ` + "`app logs -f`" + ` or a live ` + "`app stats`" + `; use ` + "`-n N`" + ` and ` + "`app stats --no-stream`" + `.

## Roll back

` + "```bash" + `
{{- if ne .RunDir "."}}
cd {{sh .RunDir}}
{{- end}}
{{- range .Destinations}}
yoho {{$.ConfigFlag}}rollback VERSION -d {{sh .Name}}
{{- end}}
` + "```" + `

Take VERSION from ` + "`yoho releases`" + `. Rollback does not revert volumes or database migrations; check that the older code works with the current data first.

## Secrets
{{if or .SecretFiles .Providers}}
{{- if .SecretFiles}}
- Files: {{range $i, $f := .SecretFiles}}{{if $i}}, {{end}}` + "`{{$f}}`" + `{{end}}
{{- end}}
{{- if .Providers}}
- Providers: {{range $i, $p := .Providers}}{{if $i}}, {{end}}` + "`{{$p.Name}}`" + ` ({{$p.Type}}){{end}}
{{- end}}
{{- if .SecretKeys}}
- Keys from providers: {{range $i, $k := .SecretKeys}}{{if $i}}, {{end}}` + "`{{$k}}`" + `{{end}}
{{- end}}
{{else}}
No secrets files or providers are configured.
{{end}}
Never print, log or commit secret values. ` + "`yoho secrets list`" + ` shows keys only. After rotating a value in the password manager, ` + "`yoho secrets rotate`" + ` restarts only the Services that changed.

## Backups
{{if .Jobs}}
` + "```bash" + `
{{- if ne .RunDir "."}}
cd {{sh .RunDir}}
{{- end}}
{{- range .Jobs}}
yoho {{$.ConfigFlag}}backup run {{sh .Name}} -d {{sh .Destination}}
yoho {{$.ConfigFlag}}backup list {{sh .Name}} -d {{sh .Destination}}
{{- end}}
` + "```" + `
{{range .Jobs}}
- ` + "`{{.Name}}`" + `: Destination ` + "`{{.Destination}}`" + `, Backup Target ` + "`{{.Target}}`" + `{{if .Schedule}}, schedule ` + "`{{.Schedule}}`" + `{{end}}
{{- end}}
{{else}}
No Backup jobs are configured.
{{end}}
## Safety

- Yoho owns only ` + "`yoho-*`" + ` containers and projects, the ` + "`yoho`" + ` network and ` + "`<root>/apps/...`" + ` on the Servers; leave everything else alone.
- Never run ` + "`docker compose down -v`" + ` or remove volumes.
- Never run ` + "`docker buildx" + " history`" + ` on a Server; it can crash dockerd.
`))

var projectHashRE = regexp.MustCompile(`(?m)^<!-- generated by yoho skill install --project; hash:([0-9a-f]{64}) -->\n`)

// renderProjectSkill renders SKILL.md. The hash comment follows the
// frontmatter (agents require frontmatter first) and covers the rest of the
// file, so a hand edit changes the computed hash.
func renderProjectSkill(p projectSkill) ([]byte, error) {
	var buf bytes.Buffer
	if err := projectSkillTmpl.Execute(&buf, p); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	end := bytes.Index(body[4:], []byte("\n---\n")) + 4 + len("\n---\n")
	sum := sha256.Sum256(body)
	out := slices.Concat(body[:end], []byte("<!-- generated by yoho skill install --project; hash:"+hex.EncodeToString(sum[:])+" -->\n"), body[end:])
	return out, nil
}

// projectSkillEdited reports whether the existing file was not written by
// --project or was changed since: its hash comment is missing or no longer
// matches the rest of the file. A missing file is not edited.
func projectSkillEdited(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	loc := projectHashRE.FindSubmatchIndex(b)
	if loc == nil {
		return true, nil
	}
	sum := sha256.Sum256(slices.Concat(b[:loc[0]], b[loc[1]:]))
	return hex.EncodeToString(sum[:]) != string(b[loc[2]:loc[3]]), nil
}

func writeProjectSkill(dest string, content []byte) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "SKILL.md"), content, 0o644)
}
