package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// rotHost runs file work locally (release.Root is a temp dir) and hands
// every docker script, with its stdin and env, to docker.
type rotHost struct {
	local  remote.Local
	mu     sync.Mutex
	calls  []rotCall
	docker func(c rotCall, stderr io.Writer) error
	swap   error // returned for the swap script, when set
}

type rotCall struct {
	Script, Stdin string
	Env           map[string]string
}

func (f *rotHost) Name() string { return "s1" }
func (f *rotHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}
func (f *rotHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if f.swap != nil && strings.Contains(c.Script, "mv -f ") {
		return "", f.swap
	}
	if !strings.Contains(c.Script, "docker ") {
		return f.local.Output(ctx, c)
	}
	call := rotCall{Script: c.Script, Env: c.Env}
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		call.Stdin = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if strings.Contains(c.Script, "docker ps") {
		return "c0ffee", nil
	}
	if f.docker == nil {
		return "", nil
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return "", f.docker(call, stderr)
}
func (f *rotHost) WriteFile(ctx context.Context, p string, data []byte, mode os.FileMode, sudo bool) error {
	return f.local.WriteFile(ctx, p, data, mode, sudo)
}
func (f *rotHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	return f.local.ReadFile(ctx, p, sudo)
}
func (f *rotHost) Close() error { return nil }

func rotDeploy(t *testing.T, compose string, ext map[string]config.ServiceExt) *plan.Deploy {
	t.Helper()
	return &plan.Deploy{App: "shop", Destination: "production", Project: loadProject(t, compose), Ext: ext}
}

const rotCompose = `
services:
  web: {image: shop-web:v1}
  db:
    image: docker.io/library/postgres:17-alpine
    environment: {POSTGRES_USER: shop}
  my: {image: mysql:8, environment: {MYSQL_USER: app}}
  ma: {image: mariadb:11, environment: {MARIADB_USER: app}}
  cache: {image: redis:7}
`

func TestPlanGeneratedRotationDetects(t *testing.T) {
	ext := map[string]config.ServiceExt{
		"web":   {Secrets: config.SecretRefs{{Name: "DATABASE_PASSWORD", Key: "POSTGRES_PASSWORD"}}},
		"db":    {Stateful: true, Generate: map[string]string{"POSTGRES_PASSWORD": "password32", "PEPPER": "hex32"}},
		"my":    {Stateful: true, Generate: map[string]string{"MYSQL_ROOT_PASSWORD": "password32", "MYSQL_PASSWORD": "password32"}},
		"ma":    {Stateful: true, Generate: map[string]string{"MARIADB_PASSWORD": "password32"}},
		"cache": {Generate: map[string]string{"SESSION_KEY": "hex64"}},
	}
	d := rotDeploy(t, rotCompose, ext)
	cases := []struct{ key, svc, strategy, user string }{
		{"POSTGRES_PASSWORD", "db", RotatePostgres, "shop"},
		{"MYSQL_ROOT_PASSWORD", "my", RotateMySQL, "root"},
		{"MYSQL_PASSWORD", "my", RotateMySQL, "app"},
		{"MARIADB_PASSWORD", "ma", RotateMySQL, "app"},
		{"SESSION_KEY", "cache", RotateNone, ""},
	}
	for _, c := range cases {
		g, err := PlanGeneratedRotation(d, c.key)
		if err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
		if g.Service != c.svc || g.Strategy != c.strategy || g.User != c.user || !g.Detected {
			t.Fatalf("%s: %+v", c.key, g)
		}
	}
	g, _ := PlanGeneratedRotation(d, "POSTGRES_PASSWORD")
	if strings.Join(g.Consumers, ",") != "db,web" {
		t.Fatalf("consumers %v", g.Consumers)
	}
	if g, _ := PlanGeneratedRotation(d, "MARIADB_PASSWORD"); !g.MariaDB {
		t.Fatal("mariadb image not detected")
	}
	if g, _ := PlanGeneratedRotation(d, "MYSQL_PASSWORD"); g.MariaDB {
		t.Fatal("mysql flagged as mariadb")
	}
	// A key of a database Service that Yoho cannot interpret needs x-yoho.rotate.
	if _, err := PlanGeneratedRotation(d, "PEPPER"); err == nil || !strings.Contains(err.Error(), "x-yoho.rotate.PEPPER.strategy") {
		t.Fatalf("PEPPER: %v", err)
	}
	if _, err := PlanGeneratedRotation(d, "NOPE"); err == nil || !strings.Contains(err.Error(), "not a generated secret") {
		t.Fatalf("NOPE: %v", err)
	}
	e := ext["db"]
	e.Rotate = map[string]config.GeneratedRotate{"PEPPER": {Strategy: "command", Command: []string{"/bin/rekey"}}}
	ext["db"] = e
	g, err := PlanGeneratedRotation(d, "PEPPER")
	if err != nil || g.Strategy != RotateCommand || g.Detected || g.Command[0] != "/bin/rekey" {
		t.Fatalf("explicit: %+v %v", g, err)
	}
	e.Rotate = map[string]config.GeneratedRotate{"POSTGRES_PASSWORD": {Strategy: "postgres", User: "bad user"}}
	ext["db"] = e
	if _, err := PlanGeneratedRotation(d, "POSTGRES_PASSWORD"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("bad user: %v", err)
	}
}

func TestPlanGeneratedRotationPostgresDefaultUserAndMySQLNeedsUser(t *testing.T) {
	d := rotDeploy(t, `
services:
  db: {image: postgres:17}
  my: {image: mysql:8}
`, map[string]config.ServiceExt{
		"db": {Generate: map[string]string{"POSTGRES_PASSWORD": "password32"}},
		"my": {Generate: map[string]string{"MYSQL_PASSWORD": "password32"}},
	})
	g, err := PlanGeneratedRotation(d, "POSTGRES_PASSWORD")
	if err != nil || g.User != "postgres" {
		t.Fatalf("%+v %v", g, err)
	}
	if _, err := PlanGeneratedRotation(d, "MYSQL_PASSWORD"); err == nil || !strings.Contains(err.Error(), "MYSQL_USER") {
		t.Fatalf("mysql without user: %v", err)
	}
}

func TestImageFamily(t *testing.T) {
	for img, want := range map[string]string{
		"postgres:17":                       RotatePostgres,
		"ghcr.io/acme/postgres@sha256:ab":   RotatePostgres,
		"postgis/postgis:16-3.4":            RotatePostgres,
		"timescale/timescaledb:latest-pg16": RotatePostgres,
		"mysql:8":                           RotateMySQL,
		"mariadb:11":                        RotateMySQL,
		"localhost:5000/redis":              "",
		"shop-web":                          "",
	} {
		if got := imageFamily(img); got != want {
			t.Errorf("%s: %q, want %q", img, got, want)
		}
	}
}

func TestRotationScriptsQuoteAndKeepValuesOut(t *testing.T) {
	const pw = "p'w\"$(touch /tmp/x)`id`"
	sql := postgresSQL(`we"ird`, pw)
	if !strings.Contains(sql, `ALTER ROLE "we""ird" WITH PASSWORD 'p''w"$(touch /tmp/x)`+"`id`';") {
		t.Fatalf("postgres quoting:\n%s", sql)
	}
	if !strings.Contains(sql, "log_min_error_statement") {
		t.Fatal("statement logging is not turned off")
	}
	my := mysqlSQL("app", `a\b'c`, false)
	if my != `ALTER USER CURRENT_USER() IDENTIFIED BY 'a\\b''c';`+"\n" {
		t.Fatalf("mysql quoting: %q", my)
	}
	if !strings.Contains(mysqlSQL("app", "x", true), "SET PASSWORD = PASSWORD('x')") {
		t.Fatal("mariadb user statement")
	}
	if root := mysqlSQL("root", "x", false); !strings.Contains(root, "'root'@'localhost'") || !strings.Contains(root, "'root'@'%'") {
		t.Fatalf("root: %q", root)
	}
	if _, err := mysqlStdin("app", "a\"b", "SELECT 1;"); err == nil {
		t.Fatal("option file accepted a double quote")
	}
	in, err := mysqlStdin("app", `s3cr\t`, "SELECT 1;\n")
	if err != nil || !strings.HasPrefix(string(in), "[client]\nuser=\"app\"\npassword=\"s3cr\\\\t\"\n"+mysqlSQLMarker+"\n") {
		t.Fatalf("option file %q %v", in, err)
	}
	for _, s := range []string{
		postgresAlterScript("c0ffee", "shop"), postgresVerifyScript("c0ffee", "shop"),
		mysqlScript("c0ffee"), commandScript("c0ffee", []string{"rekey"}), swapScript("/srv/generated/K"),
	} {
		if strings.Contains(s, pw) || strings.Contains(s, "s3cr") {
			t.Fatalf("value in script: %s", s)
		}
	}
}

func TestRotateGeneratedSecretPostgres(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	dir := release.AppDir("shop", "production")
	p := path.Join(dir, "generated", "POSTGRES_PASSWORD")
	if _, err := ensureFileOnce(ctx, h, p, []byte("OLDvalue0123456789")); err != nil {
		t.Fatal(err)
	}
	g := &GeneratedRotation{Key: "POSTGRES_PASSWORD", Kind: "password32", Service: "db", Strategy: RotatePostgres, User: "shop", Consumers: []string{"db", "web"}}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, t.Logf, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) != 32 || string(b) == "OLDvalue0123456789" {
		t.Fatalf("new value %q %v", b, err)
	}
	if _, err := os.Stat(p + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending file left: %v", err)
	}
	next := string(b)
	var alter, verify *rotCall
	for i := range h.calls {
		c := &h.calls[i]
		if strings.Contains(c.Script, next) || strings.Contains(c.Script, "OLDvalue") {
			t.Fatalf("value in script: %s", c.Script)
		}
		switch {
		case strings.Contains(c.Script, "-f -"):
			alter = c
		case strings.Contains(c.Script, "SELECT 1"):
			verify = c
		}
	}
	if alter == nil || verify == nil {
		t.Fatalf("calls: %+v", h.calls)
	}
	if !strings.Contains(alter.Stdin, "'"+next+"'") || alter.Env["PGPASSWORD"] != "OLDvalue0123456789" {
		t.Fatalf("alter call %+v", alter)
	}
	if verify.Env["PGPASSWORD"] != next {
		t.Fatal("verify did not use the new value")
	}
	log, err := os.ReadFile(RotationsLog("shop", "production"))
	if err != nil || !strings.Contains(string(log), `"fingerprint":"`) || strings.Contains(string(log), next) || strings.Contains(string(log), "OLDvalue") {
		t.Fatalf("rotation log %q %v", log, err)
	}
}

func TestRotateGeneratedSecretKeepsOldOnFailureAndResumes(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	dir := release.AppDir("shop", "production")
	p := path.Join(dir, "generated", "MYSQL_PASSWORD")
	if _, err := ensureFileOnce(ctx, h, p, []byte("OLDvalue0123456789")); err != nil {
		t.Fatal(err)
	}
	// The database echoes the failed statement, values included.
	h.docker = func(c rotCall, stderr io.Writer) error {
		io.WriteString(stderr, "ERROR 1045: "+c.Stdin)
		return &remote.ExitError{Host: "s1", Code: 1}
	}
	g := &GeneratedRotation{Key: "MYSQL_PASSWORD", Kind: "password32", Service: "db", Strategy: RotateMySQL, User: "app"}
	err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	pending, perr := os.ReadFile(p + ".next")
	if perr != nil || len(pending) != 32 {
		t.Fatalf("pending %q %v", pending, perr)
	}
	if strings.Contains(err.Error(), string(pending)) || strings.Contains(err.Error(), "OLDvalue") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error leaks a value: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "OLDvalue0123456789" {
		t.Fatalf("old value replaced: %q", b)
	}
	// The database already took the pending value: the old one is refused,
	// the pending one works, and the same value is swapped in.
	h.calls = nil
	h.docker = func(c rotCall, stderr io.Writer) error {
		if strings.Contains(c.Stdin, `password="OLDvalue`) {
			return &remote.ExitError{Host: "s1", Code: 1}
		}
		return nil
	}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, pending) {
		t.Fatalf("resume used another value")
	}
	for _, c := range h.calls {
		if strings.Contains(c.Script, string(pending)) {
			t.Fatal("value in script")
		}
	}
}

func TestRotateGeneratedSecretCommandAndNone(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	dir := release.AppDir("shop", "production")
	for _, k := range []string{"A", "B"} {
		if _, err := ensureFileOnce(ctx, h, path.Join(dir, "generated", k), []byte("old-"+k)); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	h.docker = func(c rotCall, _ io.Writer) error {
		if strings.Contains(c.Script, "rekey") {
			got = c.Stdin
		}
		return nil
	}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", &GeneratedRotation{Key: "A", Kind: "hex32", Service: "svc", Strategy: RotateCommand, Command: []string{"rekey", "--stdin"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(path.Join(dir, "generated", "A"))
	if got != "old-A\n"+string(a)+"\n" {
		t.Fatalf("command stdin %q", got)
	}
	h.calls = nil
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", &GeneratedRotation{Key: "B", Kind: "hex32", Service: "svc", Strategy: RotateNone}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("none touched a container: %+v", h.calls)
	}
	if b, _ := os.ReadFile(path.Join(dir, "generated", "B")); string(b) == "old-B" || len(b) != 64 {
		t.Fatalf("B = %q", b)
	}
	// Not generated yet: nothing to rotate.
	err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", &GeneratedRotation{Key: "C", Kind: "hex32", Strategy: RotateNone}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "yoho deploy") {
		t.Fatalf("missing: %v", err)
	}
}

func TestRotateGeneratedSecretFailedLoginCheckStillSwaps(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	p := path.Join(release.AppDir("shop", "production"), "generated", "POSTGRES_PASSWORD")
	if _, err := ensureFileOnce(ctx, h, p, []byte("OLDvalue0123456789")); err != nil {
		t.Fatal(err)
	}
	h.docker = func(c rotCall, stderr io.Writer) error {
		if strings.Contains(c.Script, "SELECT 1") {
			io.WriteString(stderr, "connection refused")
			return &remote.ExitError{Host: "s1", Code: 2}
		}
		return nil
	}
	var warns []string
	g := &GeneratedRotation{Key: "POSTGRES_PASSWORD", Kind: "password32", Service: "db", Strategy: RotatePostgres, User: "shop"}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, func(f string, a ...any) {
		warns = append(warns, fmt.Sprintf(f, a...))
	}); err != nil {
		t.Fatalf("a failed login check after ALTER must not fail the rotation: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "the database took the new password") || !strings.Contains(warns[0], "connection refused") {
		t.Fatalf("warnings %q", warns)
	}
	if b, _ := os.ReadFile(p); string(b) == "OLDvalue0123456789" || len(b) != 32 {
		t.Fatalf("not swapped: %q", b)
	}
}

func TestRotateGeneratedSecretSwapFailureIsPending(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{swap: errors.New("disk full")}
	p := path.Join(release.AppDir("shop", "production"), "generated", "POSTGRES_PASSWORD")
	if _, err := ensureFileOnce(ctx, h, p, []byte("OLDvalue0123456789")); err != nil {
		t.Fatal(err)
	}
	g := &GeneratedRotation{Key: "POSTGRES_PASSWORD", Kind: "password32", Service: "db", Strategy: RotatePostgres, User: "shop"}
	err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil)
	var pe *PendingSwapError
	if !errors.As(err, &pe) || pe.Key != "POSTGRES_PASSWORD" || !pe.Applied {
		t.Fatalf("want PendingSwapError, got %v", err)
	}
	if b, _ := os.ReadFile(p + ".next"); len(b) != 32 {
		t.Fatalf("pending value gone: %q", b)
	}
	// A re-run resumes with the pending value and finishes the swap.
	pending, _ := os.ReadFile(p + ".next")
	h.swap = nil
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, pending) {
		t.Fatal("re-run used another value")
	}
}
