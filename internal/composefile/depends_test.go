package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkSrc(t *testing.T, src, runtime string) []Finding {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	return Check(r, runtime, nil)
}

const dependsBase = `
services:
  web:
    image: web
    environment:
      DATABASE_URL: postgres://u@db:5432/x
      ANALYTICS_DATABASE_URL: postgres://u:pw@analytics-db:5432/x
    depends_on: {db: {condition: service_healthy}}
  db:
    image: pg
    healthcheck: {test: ["CMD", "true"]}
  analytics-db:
    image: pg
    healthcheck: {test: ["CMD", "true"]}
`

func TestUndeclaredDependency(t *testing.T) {
	w := msgs(checkSrc(t, dependsBase, "compose"), LevelWarning)
	for _, want := range []string{
		"web: environment ANALYTICS_DATABASE_URL refers to Service analytics-db",
		"depends_on: {analytics-db: {condition: service_healthy}}",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in:\n%s", want, w)
		}
	}
	if strings.Count(w, "refers to Service") != 1 {
		t.Errorf("declared db must not warn:\n%s", w)
	}
	sw := msgs(checkSrc(t, dependsBase, "swarm"), LevelWarning)
	if !strings.Contains(sw, "depends_on is ignored by stack deploy") {
		t.Errorf("swarm wording:\n%s", sw)
	}
}

func TestUndeclaredDependencyQuiet(t *testing.T) {
	const analytics = "postgres://u:pw@analytics-db:5432/x"
	cases := map[string]string{
		"declared": strings.Replace(dependsBase, "depends_on: {db: {condition: service_healthy}}", "depends_on: [db, analytics-db]", 1),
		// web depends on db, which depends on analytics-db.
		"transitive": strings.Replace(dependsBase, "    healthcheck: {test: [\"CMD\", \"true\"]}\n  analytics-db:", "    healthcheck: {test: [\"CMD\", \"true\"]}\n    depends_on: [analytics-db]\n  analytics-db:", 1),
		"self":       "services:\n  web:\n    image: web\n    environment: {SELF: 'http://web:3000'}\n",
		"substring":  strings.Replace(dependsBase, analytics, "postgres://u@analytics-db-backup:5432/x", 1),
		"path":       strings.Replace(dependsBase, analytics, "/var/lib/analytics-db:ro", 1),
		"no shared network": strings.Replace(dependsBase, "  analytics-db:\n    image: pg\n", "  analytics-db:\n    image: pg\n    networks: [other]\n", 1) +
			"networks:\n  other: {}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if w := msgs(checkSrc(t, src, "compose"), LevelWarning); strings.Contains(w, "refers to Service") {
				t.Errorf("unexpected warning:\n%s", w)
			}
		})
	}
}

func TestUndeclaredDependencyAliasAndStarted(t *testing.T) {
	src := `
services:
  web:
    image: web
    environment:
      CACHE: redis-primary:6379
  cache:
    image: redis
    networks:
      default: {aliases: [redis-primary]}
`
	w := msgs(checkSrc(t, src, "compose"), LevelWarning)
	if !strings.Contains(w, "refers to Service cache") || !strings.Contains(w, "{cache: {condition: service_started}}") {
		t.Errorf("alias match without healthcheck:\n%s", w)
	}
}

func TestSlowFirstProbeWarning(t *testing.T) {
	svc := func(hc string) string {
		return "services:\n  db:\n    image: pg\n    healthcheck: {test: ['CMD', 'true'], " + hc + "}\n    x-yoho: {stateful: true}\n"
	}
	if w := msgs(checkSrc(t, svc("interval: 10s"), "compose"), LevelWarning); !strings.Contains(w, "healthcheck interval 10s sets the first probe time") {
		t.Errorf("want slow-interval warning:\n%s", w)
	}
	for _, hc := range []string{"interval: 5s", "interval: 10s, start_interval: 1s", "interval: 10s, start_period: 30s"} {
		if w := msgs(checkSrc(t, svc(hc), "compose"), LevelWarning); strings.Contains(w, "first probe") {
			t.Errorf("%s must not warn:\n%s", hc, w)
		}
	}
	plain := "services:\n  w:\n    image: w\n    healthcheck: {test: ['CMD', 'true'], interval: 10s}\n"
	if w := msgs(checkSrc(t, plain, "compose"), LevelWarning); strings.Contains(w, "first probe") {
		t.Errorf("plain Service must not warn:\n%s", w)
	}
}
