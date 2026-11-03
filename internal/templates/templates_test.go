package templates

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("YOHO_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (YOHO_UPDATE_GOLDEN=1): %v\n%s", path, err, got)
	}
	if string(want) != string(got) {
		t.Fatalf("golden %s\n--- got ---\n%s", path, got)
	}
}

func TestEachTemplateGolden(t *testing.T) {
	for _, n := range Names() {
		t.Run(n, func(t *testing.T) {
			res, err := Merge(nil, []string{n})
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, n+".golden.yaml", res.Compose)
			if len(res.Services) != 1 || res.Services[0] != n {
				t.Fatalf("services %v", res.Services)
			}
		})
	}
	res, err := Merge(nil, Names())
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "all.golden.yaml", res.Compose)
}

// TestTemplateRules checks what every template promises: a pinned image, no
// published ports, stateful, a healthcheck test, and a Backup.
func TestTemplateRules(t *testing.T) {
	for _, tm := range List() {
		src, err := Source(tm.Name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Image       string         `yaml:"image"`
				Ports       []any          `yaml:"ports"`
				Restart     string         `yaml:"restart"`
				Healthcheck map[string]any `yaml:"healthcheck"`
				XYoho       struct {
					Stateful bool              `yaml:"stateful"`
					Generate map[string]string `yaml:"generate"`
					Backup   map[string]any    `yaml:"backup"`
				} `yaml:"x-yoho"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(src, &doc); err != nil {
			t.Fatal(err)
		}
		s, ok := doc.Services[tm.Name]
		if !ok || len(doc.Services) != 1 {
			t.Fatalf("%s: services %v", tm.Name, doc.Services)
		}
		if s.Image != tm.Image || strings.HasSuffix(s.Image, ":latest") || !strings.Contains(s.Image, ":") {
			t.Errorf("%s: image %q (List says %q)", tm.Name, s.Image, tm.Image)
		}
		if len(s.Ports) > 0 {
			t.Errorf("%s publishes ports", tm.Name)
		}
		if s.Restart != "unless-stopped" {
			t.Errorf("%s: restart %q", tm.Name, s.Restart)
		}
		if s.Healthcheck["test"] == nil {
			t.Errorf("%s: no healthcheck test", tm.Name)
		}
		if !s.XYoho.Stateful || len(s.XYoho.Backup) == 0 {
			t.Errorf("%s: stateful %v backup %v", tm.Name, s.XYoho.Stateful, s.XYoho.Backup)
		}
		for _, k := range tm.Secrets {
			if s.XYoho.Generate[k] == "" {
				t.Errorf("%s: secret %s is not generated", tm.Name, k)
			}
		}
		if len(s.XYoho.Generate) != len(tm.Secrets) {
			t.Errorf("%s: generate %v, List says %v", tm.Name, s.XYoho.Generate, tm.Secrets)
		}
	}
}

func TestMergeKeepsExistingFile(t *testing.T) {
	existing := `# my app
name: shop

services:
    web:
        image: nginx:1.27-alpine
        restart: unless-stopped
        # trailing comment inside web

# volumes used by web
volumes:
    assets: {}

# end of file
`
	res, err := Merge([]byte(existing), []string{"postgres", "redis"})
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "merge.golden.yaml", res.Compose)
	if !strings.HasPrefix(string(res.Compose), "# my app\nname: shop\n\nservices:\n    web:\n") {
		t.Fatalf("existing text changed:\n%s", res.Compose)
	}
	if !strings.Contains(string(res.Compose), "\n    postgres:\n        image: postgres:17-alpine\n") {
		t.Fatalf("not reindented to the file's 4 spaces:\n%s", res.Compose)
	}
}

func TestMergeShapes(t *testing.T) {
	cases := map[string]string{
		"no volumes":    "services:\n  web:\n    image: nginx:1.27-alpine\n",
		"null services": "name: x\nservices:\nvolumes:\n  other:\n",
		"volumes first": "volumes:\n  other: {}\nservices:\n  web:\n    image: nginx:1.27-alpine\n",
		"crlf":          "services:\r\n  web:\r\n    image: nginx:1.27-alpine\r\n",
		"no newline":    "services:\n  web:\n    image: nginx:1.27-alpine",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Merge([]byte(src), []string{"rustfs"})
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Services map[string]any `yaml:"services"`
				Volumes  map[string]any `yaml:"volumes"`
			}
			if err := yaml.Unmarshal(res.Compose, &doc); err != nil {
				t.Fatalf("%v\n%s", err, res.Compose)
			}
			if _, ok := doc.Services["rustfs"]; !ok {
				t.Fatalf("no rustfs:\n%s", res.Compose)
			}
			if _, ok := doc.Volumes["rustfsdata"]; !ok {
				t.Fatalf("no rustfsdata:\n%s", res.Compose)
			}
			if !keepsLines(src, string(res.Compose)) {
				t.Fatalf("existing text changed:\n%q", res.Compose)
			}
			if strings.Contains(src, "\r\n") && strings.Count(string(res.Compose), "\n") != strings.Count(string(res.Compose), "\r\n") {
				t.Fatalf("mixed line endings:\n%q", res.Compose)
			}
		})
	}
}

// keepsLines reports whether every line of before appears in after, in order.
func keepsLines(before, after string) bool {
	before = strings.ReplaceAll(before, "\r\n", "\n")
	rest := strings.Split(strings.ReplaceAll(after, "\r\n", "\n"), "\n")
	for _, l := range strings.Split(strings.TrimRight(before, "\n"), "\n") {
		for {
			if len(rest) == 0 {
				return false
			}
			cur := rest[0]
			rest = rest[1:]
			if cur == l {
				break
			}
		}
	}
	return true
}

func TestMergeRefusesConflicts(t *testing.T) {
	cases := map[string]string{
		"service":  "services:\n  postgres:\n    image: postgres:16\n",
		"volume":   "services:\n  db:\n    image: postgres:16\n    volumes: [pgdata:/data]\nvolumes:\n  pgdata:\n",
		"flow":     "services: {web: {image: nginx}}\n",
		"two docs": "services:\n  web:\n    image: nginx\n---\nservices: {}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Merge([]byte(src), []string{"postgres"})
			var te *Error
			if !errors.As(err, &te) || te.Hint == "" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestParse(t *testing.T) {
	got, err := Parse([]string{"postgres, Redis", "postgres", "rustfs"})
	if err != nil || strings.Join(got, ",") != "postgres,redis,rustfs" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := Parse([]string{"mysql"}); err == nil || !strings.Contains(err.(*Error).Hint, "clickhouse") {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := Parse([]string{""}); err == nil {
		t.Fatal("empty accepted")
	}
}
