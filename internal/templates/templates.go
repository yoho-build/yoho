// Package templates holds the compose Services `yoho init --template` writes:
// ready-to-deploy Stateful Services (Postgres, Redis, ClickHouse, RustFS) with
// x-yoho blocks for generated secrets and Backups, and no published ports.
//
// Merge appends the chosen Services and their volumes to an existing compose
// file as text, so the operator's comments and layout stay as they were.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed files/*.yaml
var files embed.FS

// Template is one Service template.
type Template struct {
	Name        string
	Description string
	Image       string
	// Connect says how other Services reach it.
	Connect string
	// Secrets are the generated secret keys a connecting Service lists in
	// x-yoho.secrets.
	Secrets []string
}

var all = []Template{
	{Name: "postgres", Description: "PostgreSQL 17, pg_dump Backup", Image: "postgres:17-alpine",
		Connect: "postgres:5432, user postgres, database postgres", Secrets: []string{"POSTGRES_PASSWORD"}},
	{Name: "redis", Description: "Valkey (Redis-compatible), append-only file, volume Backup", Image: "valkey/valkey:9.0-alpine",
		Connect: "redis://redis:6379 (no password, compose network only)"},
	{Name: "clickhouse", Description: "ClickHouse 26.8 LTS, volume Backup", Image: "clickhouse/clickhouse-server:26.8-alpine",
		Connect: "http://clickhouse:8123 or clickhouse:9000 (native), user default", Secrets: []string{"CLICKHOUSE_PASSWORD"}},
	{Name: "rustfs", Description: "RustFS S3-compatible object storage, volume Backup", Image: "rustfs/rustfs:1.0.4",
		Connect: "S3 http://rustfs:9000 (path-style, region us-east-1)", Secrets: []string{"RUSTFS_ACCESS_KEY", "RUSTFS_SECRET_KEY"}},
}

// List returns every template in display order.
func List() []Template { return append([]Template(nil), all...) }

// Lookup returns the named template.
func Lookup(name string) (Template, bool) {
	for _, t := range all {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Names returns the template names.
func Names() []string {
	out := make([]string, len(all))
	for i, t := range all {
		out[i] = t.Name
	}
	return out
}

// Error is a problem the operator fixes. Hint is the next step.
type Error struct {
	Message string
	Hint    string
}

func (e *Error) Error() string { return e.Message }

// Parse splits and checks names ("postgres,redis" or repeated flags). It
// drops duplicates and keeps the order given.
func Parse(values []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, n := range strings.Split(v, ",") {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" {
				continue
			}
			if _, ok := Lookup(n); !ok {
				return nil, &Error{Message: fmt.Sprintf("unknown template %q", n), Hint: "available: " + strings.Join(Names(), ", ") + " (yoho init --list-templates)"}
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return nil, &Error{Message: "--template is empty", Hint: "available: " + strings.Join(Names(), ", ")}
	}
	return out, nil
}

// part is a template split into its services and volumes blocks. Lines keep
// the template's two-space indentation.
type part struct {
	services []string // lines under services:
	volumes  []string // lines under volumes:
	svcNames []string
	volNames []string
}

func load(name string) (part, error) {
	b, err := files.ReadFile("files/" + name + ".yaml")
	if err != nil {
		return part{}, err
	}
	var p part
	var cur *[]string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch line {
		case "services:":
			cur = &p.services
			continue
		case "volumes:":
			cur = &p.volumes
			continue
		}
		if cur == nil {
			return part{}, fmt.Errorf("template %s: content before services:", name)
		}
		*cur = append(*cur, line)
	}
	var doc struct {
		Services map[string]any `yaml:"services"`
		Volumes  map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return part{}, fmt.Errorf("template %s: %w", name, err)
	}
	p.svcNames, p.volNames = sortedKeys(doc.Services), sortedKeys(doc.Volumes)
	return p, nil
}

// Source returns the template's compose file as embedded.
func Source(name string) ([]byte, error) { return files.ReadFile("files/" + name + ".yaml") }

// Result is the merged compose file and what was added.
type Result struct {
	Compose  []byte
	Services []string
	Volumes  []string
}

const header = "# compose.yaml — Services from `yoho init --template`. Review, then `yoho config check`.\n"

// Merge appends the named templates' Services and volumes to existing (a
// compose file, or empty for a new one). A Service or volume name that
// already exists is refused; nothing is overwritten.
func Merge(existing []byte, names []string) (*Result, error) {
	var parts []part
	res := &Result{}
	for _, n := range names {
		p, err := load(n)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
		res.Services = append(res.Services, p.svcNames...)
		res.Volumes = append(res.Volumes, p.volNames...)
	}
	var svcLines, volLines []string
	for i, p := range parts {
		if i > 0 {
			svcLines = append(svcLines, "")
		}
		svcLines = append(svcLines, p.services...)
		volLines = append(volLines, p.volumes...)
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		var b strings.Builder
		b.WriteString(header)
		b.WriteString("services:\n" + strings.Join(svcLines, "\n") + "\n")
		b.WriteString("\nvolumes:\n" + strings.Join(volLines, "\n") + "\n")
		res.Compose = []byte(b.String())
		return res, verify(nil, res)
	}
	out, err := insert(existing, res, svcLines, volLines)
	if err != nil {
		return nil, err
	}
	res.Compose = out
	return res, verify(existing, res)
}

// block is one top-level key of the existing file.
type block struct {
	present bool
	null    bool // "services:" with no value
	keyLine int  // 1-based
	indent  int  // column of the child keys, 0-based
	end     int  // insert after this 1-based line
	names   map[string]bool
}

func insert(existing []byte, res *Result, svcLines, volLines []string) ([]byte, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(existing))
	if err := dec.Decode(&doc); err != nil {
		return nil, &Error{Message: "compose file: " + err.Error(), Hint: "fix the YAML, then run yoho init --template again"}
	}
	var extra yaml.Node
	if dec.Decode(&extra) == nil {
		return nil, &Error{Message: "compose file has more than one YAML document", Hint: "add the Services by hand; templates are in yoho init --list-templates"}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Style&yaml.FlowStyle != 0 {
		return nil, &Error{Message: "compose file is not a block-style mapping", Hint: "use block style (services: on its own line)"}
	}
	text := string(existing)
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	root := doc.Content[0]
	find := func(key string) (block, error) {
		var b block
		for i := 0; i+1 < len(root.Content); i += 2 {
			k, v := root.Content[i], root.Content[i+1]
			if k.Value != key {
				continue
			}
			b.present, b.keyLine, b.names = true, k.Line, map[string]bool{}
			next := len(lines) + 1
			if i+2 < len(root.Content) {
				next = root.Content[i+2].Line
			}
			b.end = lastContent(lines, k.Line, next)
			switch {
			case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
				b.null, b.indent, b.end = true, 2, k.Line
			case v.Kind != yaml.MappingNode || v.Style&yaml.FlowStyle != 0:
				return b, &Error{Message: key + " is not a block-style mapping", Hint: "write " + key + ": with one entry per line, then run yoho init --template again"}
			case len(v.Content) == 0:
				b.null, b.indent, b.end = true, 2, k.Line
			default:
				b.indent = v.Content[0].Column - 1
				for j := 0; j < len(v.Content); j += 2 {
					b.names[v.Content[j].Value] = true
				}
			}
			return b, nil
		}
		return b, nil
	}
	svc, err := find("services")
	if err != nil {
		return nil, err
	}
	vol, err := find("volumes")
	if err != nil {
		return nil, err
	}
	var taken []string
	for _, n := range res.Services {
		if svc.names[n] {
			taken = append(taken, "service "+n)
		}
	}
	for _, n := range res.Volumes {
		if vol.names[n] {
			taken = append(taken, "volume "+n)
		}
	}
	if len(taken) > 0 {
		return nil, &Error{
			Message: "compose file already has " + strings.Join(taken, ", "),
			Hint:    "rename or remove the existing entry, or leave that template out; yoho init never overwrites a Service",
		}
	}
	type ins struct {
		after int
		lines []string
	}
	var edits []ins
	add := func(b block, key string, body []string, gap bool) {
		if !b.present {
			edits = append(edits, ins{len(lines), append([]string{"", key + ":"}, body...)})
			return
		}
		body = reindent(body, b.indent)
		if gap && !b.null {
			body = append([]string{""}, body...)
		}
		edits = append(edits, ins{b.end, body})
	}
	add(svc, "services", svcLines, true)
	add(vol, "volumes", volLines, false)
	// Apply the later insertion first so earlier line numbers stay valid; at
	// the same line, apply volumes first so services end up above them.
	if len(edits) == 2 && edits[0].after <= edits[1].after {
		edits[0], edits[1] = edits[1], edits[0]
	}
	for _, e := range edits {
		tail := append([]string(nil), lines[e.after:]...)
		lines = append(append(lines[:e.after], e.lines...), tail...)
	}
	return []byte(strings.Join(lines, nl) + nl), nil
}

// lastContent is the last line in (keyLine, next) that is not blank and not
// a top-level comment (those usually introduce the next key), or keyLine.
func lastContent(lines []string, keyLine, next int) int {
	for l := next - 1; l > keyLine; l-- {
		s := lines[l-1]
		if strings.TrimSpace(s) == "" || strings.HasPrefix(s, "#") {
			continue
		}
		return l
	}
	return keyLine
}

// reindent turns the template's two-space levels into indent spaces per
// level, so the new Services line up with the file's own.
func reindent(body []string, indent int) []string {
	out := make([]string, len(body))
	for i, l := range body {
		trimmed := strings.TrimLeft(l, " ")
		if trimmed != "" {
			l = strings.Repeat(" ", (len(l)-len(trimmed))/2*indent) + trimmed
		}
		out[i] = l
	}
	return out
}

// verify re-reads the result: every old and new Service and volume is there.
func verify(existing []byte, res *Result) error {
	var before, after struct {
		Services map[string]any `yaml:"services"`
		Volumes  map[string]any `yaml:"volumes"`
	}
	if existing != nil {
		if err := yaml.Unmarshal(existing, &before); err != nil {
			return err
		}
	}
	if err := yaml.Unmarshal(res.Compose, &after); err != nil {
		return &Error{Message: "merged compose file does not parse: " + err.Error(), Hint: "add the Services by hand; nothing was written"}
	}
	want := map[string]map[string]bool{"services": {}, "volumes": {}}
	for n := range before.Services {
		want["services"][n] = true
	}
	for n := range before.Volumes {
		want["volumes"][n] = true
	}
	for _, n := range res.Services {
		want["services"][n] = true
	}
	for _, n := range res.Volumes {
		want["volumes"][n] = true
	}
	got := map[string]map[string]any{"services": after.Services, "volumes": after.Volumes}
	for kind, names := range want {
		if len(got[kind]) != len(names) {
			return &Error{Message: fmt.Sprintf("merged compose file has %d %s, want %d", len(got[kind]), kind, len(names)), Hint: "add the Services by hand; nothing was written"}
		}
		for n := range names {
			if _, ok := got[kind][n]; !ok {
				return &Error{Message: "merged compose file lost " + kind + " " + n, Hint: "add the Services by hand; nothing was written"}
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
