package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDrillProjectNaming(t *testing.T) {
	p, err := newDrillProject("shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, "yoho-shop-production-drill-") || !ValidDrillProject("shop", "production", p) {
		t.Fatalf("project %q", p)
	}
	if q, _ := newDrillProject("shop", "production"); q == p {
		t.Fatal("drill projects must be unique")
	}
	for _, bad := range []string{
		"yoho-shop-production",                // the App itself
		"yoho-shop-production-drill-",         // no suffix
		"yoho-shop-production-drill-ABCDEF12", // not lowercase hex
		"yoho-shop-production-drill-1234567",  // short
		"yoho-shop-production-drill-123456789",
		"yoho-shop-staging-drill-12345678", // another Destination
		"yoho-blog-production-drill-12345678",
		"yoho-shop-production-drill-1234'; rm -rf / #",
		"yoho-shop-production_pgdata",
	} {
		if ValidDrillProject("shop", "production", bad) {
			t.Errorf("%q accepted as a drill project", bad)
		}
		if _, err := DrillCleanupScript("shop", "production", bad); err == nil {
			t.Errorf("cleanup script built for %q", bad)
		}
	}
}

const compiledRelease = `name: yoho-shop-production
services:
  db:
    image: postgres:17-alpine
    container_name: shop-db
    privileged: true
    network_mode: host
    ports:
      - {target: 5432, published: "5432", protocol: tcp}
    labels: {yoho.app: shop, yoho.destination: production, yoho.service: db}
    networks: {default: null, yoho: null}
    depends_on: {cache: {condition: service_started, required: true}}
    restart: unless-stopped
    environment: {POSTGRES_USER: app, POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD}
    env_file:
      - {path: /var/lib/yoho/apps/shop/production/secrets/g1/db.env, required: true}
    healthcheck: {test: [CMD, pg_isready, -h, 127.0.0.1], interval: 2s}
    volumes:
      - {type: volume, source: pgdata, target: /var/lib/postgresql/data}
      - {type: bind, source: /var/lib/yoho/apps/shop/production/files/init.sql, target: /docker-entrypoint-initdb.d/init.sql, read_only: true}
      - {type: tmpfs, target: /scratch}
    secrets:
      - {source: db.POSTGRES_PASSWORD, target: POSTGRES_PASSWORD}
  web:
    image: shop:v1
    ports: ["80:80"]
volumes:
  pgdata:
    name: yoho-shop-production_pgdata
    driver: local
    driver_opts: {type: none, o: bind, device: /srv/pgdata}
secrets:
  db.POSTGRES_PASSWORD: {file: /var/lib/yoho/apps/shop/production/secrets/g1/db/POSTGRES_PASSWORD}
networks:
  yoho: {external: true, name: yoho}
`

func TestDrillComposeIsolation(t *testing.T) {
	p := DrillProject("shop", "production", "0a1b2c3d")
	out, err := DrillCompose([]byte(compiledRelease), "shop", "production", p, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["name"] != p {
		t.Fatalf("name %v", doc["name"])
	}
	svcs := doc["services"].(map[string]any)
	if _, ok := svcs["web"]; ok || len(svcs) != 1 {
		t.Fatalf("only the drilled Services run: %v", svcs)
	}
	db := svcs["db"].(map[string]any)
	for _, k := range []string{"ports", "container_name", "privileged", "network_mode", "depends_on"} {
		if _, ok := db[k]; ok {
			t.Errorf("drill keeps %s", k)
		}
	}
	if db["restart"] != "no" || db["image"] != "postgres:17-alpine" || db["healthcheck"] == nil || db["env_file"] == nil {
		t.Errorf("service %v", db)
	}
	labels := db["labels"].(map[string]any)
	if labels[LabelDrill] != p || labels["yoho.app"] != nil || labels["yoho.destination"] != nil || len(labels) != 3 {
		t.Errorf("labels %v", labels)
	}
	nets := db["networks"].(map[string]any)
	if _, ok := nets["yoho"]; ok || len(nets) != 1 {
		t.Errorf("networks %v", nets)
	}
	def := doc["networks"].(map[string]any)["default"].(map[string]any)
	if def["internal"] != true {
		t.Errorf("drill network must be internal: %v", def)
	}
	if _, ok := doc["networks"].(map[string]any)["yoho"]; ok {
		t.Error("drill joins the Proxy network")
	}
	vol := doc["volumes"].(map[string]any)["pgdata"].(map[string]any)
	if len(vol) != 1 || vol["labels"].(map[string]any)[LabelDrill] != p {
		t.Errorf("drill volume must be fresh and labelled only: %v", vol)
	}
	if !strings.Contains(string(out), "read_only: true") || !strings.Contains(string(out), "create_host_path: false") {
		t.Errorf("bind mount not read-only:\n%s", out)
	}
	sec := doc["secrets"].(map[string]any)["db.POSTGRES_PASSWORD"].(map[string]any)
	if len(sec) != 1 || sec["file"] == nil {
		t.Errorf("secret %v", sec)
	}
	for _, s := range []string{"5432:", "/srv/pgdata", "yoho-shop-production_pgdata", "unless-stopped", "shop-db"} {
		if strings.Contains(string(out), s) {
			t.Errorf("drill compose contains %q:\n%s", s, out)
		}
	}
}

func TestDrillComposeRefusals(t *testing.T) {
	p := DrillProject("shop", "production", "0a1b2c3d")
	for name, tc := range map[string]struct{ yaml, want string }{
		"writable bind": {`services: {db: {image: pg, volumes: [{type: bind, source: /srv/data, target: /data}]}}`, "read-write"},
		"env secret":    {`services: {db: {image: pg, secrets: [{source: s}]}}` + "\nsecrets: {s: {environment: PW}}", "file based"},
		"short volume":  {`services: {db: {image: pg, volumes: ["/srv:/data"]}}`, "short volume"},
		"unknown vol":   {`services: {db: {image: pg, volumes: [{type: volume, source: other, target: /d}]}}`, "not a named volume"},
		"missing":       {`services: {web: {image: x}}`, "not in the deployed Release"},
		"no image":      {`services: {db: {build: .}}`, "no image"},
		"npipe":         {`services: {db: {image: pg, volumes: [{type: npipe, source: x, target: /d}]}}`, "not supported"},
	} {
		if _, err := DrillCompose([]byte(tc.yaml), "shop", "production", p, []string{"db"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := DrillCompose([]byte(compiledRelease), "shop", "production", "yoho-shop-production", []string{"db"}); err == nil {
		t.Error("the App's own project accepted as a drill project")
	}
}

// fakeDocker answers the docker commands of DrillCleanupScript from a table:
//
//	c <id> <project> <labels> <volumes,comma>
//	v <name> <project> <labels>
//	n <id> <name> <project> <labels>
//
// where labels is drill|drill.app|drill.destination|yoho.app|yoho.destination.
const fakeDocker = `#!/bin/sh
db=$FAKE_DB
case "$1" in
ps) f=$5
  case "$f" in
  label=com.docker.compose.project=*) p=${f#label=com.docker.compose.project=}; awk -v p="$p" '$1=="c" && $3==p {print $2}' "$db";;
  volume=*) v=${f#volume=}; awk -v v="$v" '$1=="c" && index(","$5",", ","v",") {print $2}' "$db";;
  esac;;
inspect) awk -v id="$4" '$1=="c" && $2==id {print $4}' "$db";;
volume)
  case "$2" in
  ls) p=${5#label=com.docker.compose.project=}; awk -v p="$p" '$1=="v" && $3==p {print $2}' "$db";;
  inspect) awk -v n="$5" '$1=="v" && $2==n {print $4}' "$db";;
  rm) echo "volume rm $3" >>"$FAKE_LOG";;
  esac;;
network)
  case "$2" in
  ls) p=${6#label=com.docker.compose.project=}; awk -v p="$p" '$1=="n" && $4==p {print $2}' "$db";;
  inspect) if [ "$4" = '{{.Name}}' ]; then awk -v i="$5" '$1=="n" && $2==i {print $3}' "$db"; else awk -v i="$5" '$1=="n" && $2==i {print $5}' "$db"; fi;;
  rm) echo "network rm $3" >>"$FAKE_LOG";;
  esac;;
rm) shift 3; echo "rm $*" >>"$FAKE_LOG";;
esac
`

func runCleanup(t *testing.T, project, table string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	db, log := filepath.Join(dir, "db"), filepath.Join(dir, "log")
	if err := os.WriteFile(db, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := DrillCleanupScript("shop", "production", project)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DB="+db, "FAKE_LOG="+log)
	out, runErr := cmd.CombinedOutput()
	removed, _ := os.ReadFile(log)
	if runErr != nil {
		return string(removed), &exec.ExitError{Stderr: out}
	}
	return string(removed), nil
}

func TestDrillCleanupRemovesOnlyDrillOwnedObjects(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	p := DrillProject("shop", "production", "0a1b2c3d")
	own := p + "|shop|production||"
	app := "||||shop|production"
	real := "c app1 yoho-shop-production " + app + " yoho-shop-production_pgdata\n" +
		"v yoho-shop-production_pgdata yoho-shop-production " + app + "\n"
	drill := "c d1 " + p + " " + own + " " + p + "_pgdata\n" +
		"v " + p + "_pgdata " + p + " " + own + "\n" +
		"n net1 " + p + "_default " + p + " " + own + "\n"

	removed, err := runCleanup(t, p, real+drill)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	want := "rm d1\nvolume rm " + p + "_pgdata\nnetwork rm net1\n"
	if removed != want {
		t.Fatalf("removed:\n%s\nwant:\n%s", removed, want)
	}

	for name, table := range map[string]string{
		"App container in drill project": real + drill + "c app2 " + p + " " + app + " \n",
		"unlabelled container":           real + drill + "c x " + p + " |||| \n",
		"other drill's label":            real + drill + "c x " + p + " " + DrillProject("shop", "production", "ffffffff") + "|shop|production|| \n",
		"App volume in drill project":    real + drill + "v yoho-shop-production_pgdata2 " + p + " " + own + "\n",
		"foreign-named volume":           real + drill + "v pgdata " + p + " " + own + "\n",
		"unlabelled volume":              real + drill + "v " + p + "_x " + p + " ||||\n",
		"volume used by the App":         real + "c d1 " + p + " " + own + " " + p + "_pgdata\nc app9 yoho-shop-production " + app + " " + p + "_pgdata\nv " + p + "_pgdata " + p + " " + own + "\n",
		"foreign network":                real + drill + "n net2 yoho_default " + p + " " + own + "\n",
		"unlabelled network":             real + drill + "n net3 " + p + "_x " + p + " ||||\n",
	} {
		removed, err := runCleanup(t, p, table)
		if err == nil {
			t.Errorf("%s: cleanup succeeded", name)
		}
		if removed != "" {
			t.Errorf("%s: removed objects before refusing:\n%s", name, removed)
		}
	}
}

func TestRemoveDrillNeverRunsForTheApp(t *testing.T) {
	h := &fakeHost{}
	for _, p := range []string{"yoho-shop-production", "yoho-shop-staging-drill-12345678", ""} {
		if err := RemoveDrill(context.Background(), h, "shop", "production", p); err == nil {
			t.Errorf("RemoveDrill(%q) accepted", p)
		}
	}
	if len(h.cmds) != 0 {
		t.Fatalf("ran commands:\n%s", h.scripts())
	}
}

func TestDrillVolumeRefusesAppVolume(t *testing.T) {
	p := DrillProject("shop", "production", "0a1b2c3d")
	for name, tc := range map[string]struct{ name, labels string }{
		"App volume":       {"yoho-shop-production_pgdata", p + "|"},
		"unlabelled":       {p + "_pgdata", "|"},
		"App labels":       {p + "_pgdata", p + "|shop"},
		"other drill name": {DrillProject("shop", "production", "ffffffff") + "_pgdata", p + "|"},
	} {
		h := &fakeHost{respond: func(_ context.Context, s string) (string, error) {
			if strings.HasPrefix(s, "docker volume ls") {
				return tc.name, nil
			}
			if strings.Contains(s, "docker volume inspect") {
				return tc.labels, nil
			}
			return "", nil
		}}
		if _, err := drillVolume(context.Background(), h, "shop", "production", p, "pgdata"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h := &fakeHost{respond: func(_ context.Context, s string) (string, error) {
		if strings.HasPrefix(s, "docker volume ls") {
			return p + "_pgdata", nil
		}
		return p + "|", nil
	}}
	if n, err := drillVolume(context.Background(), h, "shop", "production", p, "pgdata"); err != nil || n != p+"_pgdata" {
		t.Fatalf("drill volume: %q %v", n, err)
	}
}

func TestDrillRefusesSwarmBeforeTouchingTheServer(t *testing.T) {
	h := &fakeHost{}
	_, err := Drill(context.Background(), DrillOptions{App: "shop", Destination: "production", Host: h, ID: "latest", Runtime: "swarm"})
	if err == nil || len(h.cmds) != 0 {
		t.Fatalf("err %v, %d commands", err, len(h.cmds))
	}
}

func TestDrillScriptsParse(t *testing.T) {
	s, err := DrillCleanupScript("shop", "production", DrillProject("shop", "production", "0a1b2c3d"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}
