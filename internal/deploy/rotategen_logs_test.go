package deploy

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

func TestSCRAMVerifierMatchesPostgres(t *testing.T) {
	// From postgres:17: CREATE ROLE vec PASSWORD 'pencil', then pg_authid.
	const want = "SCRAM-SHA-256$4096:0qlE46l70ueEmzFxOdUTZA==$Eg2tWsqll1XjF5mrk3VDmJ+6z9PRNzP6wrzqqSqUETY=:E1sjLG/yUXKHS3LKQaPmOqSS+hoY/FY7oqb37y/uuWc="
	salt, _ := base64.StdEncoding.DecodeString("0qlE46l70ueEmzFxOdUTZA==")
	got, err := scramVerifierWithSalt("pencil", salt, 4096)
	if err != nil || got != want {
		t.Fatalf("verifier %s %v", got, err)
	}
	a, _ := scramSHA256Verifier("pencil")
	b, _ := scramSHA256Verifier("pencil")
	if a == b || !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") {
		t.Fatalf("salt not random: %s %s", a, b)
	}
	for _, bad := range []string{"", "café", "a\nb"} {
		if _, err := scramSHA256Verifier(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// SASLprep is skipped: every generator must produce printable ASCII.
	for _, kind := range []string{"password32", "password64", "hex32", "hex64", "base64_32", "base64_64"} {
		v, err := secrets.Generate(kind)
		if err != nil || !printableASCII(v) {
			t.Fatalf("%s: generated value is not printable ASCII", kind)
		}
	}
}

func TestMySQLNativeHash(t *testing.T) {
	// PASSWORD('password') in MySQL 5.7 / MariaDB.
	if got := mysqlNativeHash("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Fatal(got)
	}
}

func TestMySQLRotationSQLKeepsPlaintextOutOfLogs(t *testing.T) {
	const pw = "NEWsecret0123"
	hash := mysqlNativeHash(pw)
	cases := []struct {
		name string
		user string
		p    mysqlProbe
		want string // substring; "" means refused
		bare bool   // the plaintext may be in the statement
	}{
		{"mariadb native, no log_off", "app", mysqlProbe{MariaDB: true, GeneralLog: true, Native: true}, "SET PASSWORD = '" + hash + "';", false},
		{"mariadb native, log_off", "app", mysqlProbe{MariaDB: true, GeneralLog: true, Native: true, LogOff: true}, "SET SESSION sql_log_off=1;\nSET PASSWORD = '" + hash + "';", false},
		{"mariadb root native", "root", mysqlProbe{MariaDB: true, Native: true, LogOff: true}, "'root'@'%' IDENTIFIED BY PASSWORD '" + hash + "'", false},
		{"mariadb ed25519, general log, no log_off", "app", mysqlProbe{MariaDB: true, GeneralLog: true}, "", false},
		{"mariadb ed25519, slow log", "app", mysqlProbe{MariaDB: true, SlowLog: true, LogOff: true}, "", false},
		{"mariadb ed25519, log_off", "app", mysqlProbe{MariaDB: true, GeneralLog: true, LogOff: true}, "SET SESSION sql_log_off=1;\nSET PASSWORD = PASSWORD(", true},
		{"mariadb ed25519, logs off", "app", mysqlProbe{MariaDB: true}, "SET PASSWORD = PASSWORD(", true},
		{"mysql general log (rewritten)", "app", mysqlProbe{GeneralLog: true}, "ALTER USER CURRENT_USER() IDENTIFIED BY", true},
		{"mysql log_raw, no log_off", "app", mysqlProbe{GeneralLog: true, LogRaw: true}, "", false},
		{"mysql log_raw, log_off", "app", mysqlProbe{GeneralLog: true, LogRaw: true, LogOff: true}, "SET SESSION sql_log_off=1;\nALTER USER", true},
	}
	for _, c := range cases {
		sql, err := mysqlRotationSQL(c.user, pw, c.p)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: not refused: %s", c.name, sql)
			} else if strings.Contains(err.Error(), pw) {
				t.Errorf("%s: value in error", c.name)
			}
			continue
		}
		if err != nil || !strings.Contains(sql, c.want) {
			t.Errorf("%s: %q %v", c.name, sql, err)
		}
		if !c.bare && strings.Contains(sql, pw) {
			t.Errorf("%s: plaintext in %q", c.name, sql)
		}
	}
}

func TestRotateGeneratedSecretMariaDBSendsHash(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	p := path.Join(release.AppDir("shop", "production"), "generated", "MARIADB_PASSWORD")
	if _, err := ensureFileOnce(ctx, h, p, []byte("OLDvalue0123456789")); err != nil {
		t.Fatal(err)
	}
	h.out = func(c rotCall) string {
		switch {
		case strings.Contains(c.Stdin, "SHOW GLOBAL VARIABLES"):
			return "general_log\tON\nslow_query_log\tOFF\nversion\t11.4.2-MariaDB-ubu2404\n"
		case strings.Contains(c.Stdin, "SHOW CREATE USER"):
			return "CREATE USER `app`@`%` IDENTIFIED BY PASSWORD '*0000000000000000000000000000000000000000'\n"
		}
		return ""
	}
	h.docker = func(c rotCall, stderr io.Writer) error {
		if strings.Contains(c.Stdin, "sql_log_off") && !strings.Contains(c.Stdin, "SET PASSWORD") {
			io.WriteString(stderr, "ERROR 1227: Access denied; you need the SUPER privilege")
			return &remote.ExitError{Host: "s1", Code: 1}
		}
		return nil
	}
	// The plan said MySQL; the server's version says MariaDB.
	g := &GeneratedRotation{Key: "MARIADB_PASSWORD", Kind: "password32", Service: "db", Strategy: RotateMySQL, User: "app"}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil); err != nil {
		t.Fatal(err)
	}
	next, _ := os.ReadFile(p)
	var set string
	for _, c := range h.calls {
		_, sql, _ := strings.Cut(c.Stdin, mysqlSQLMarker+"\n")
		if strings.Contains(sql, string(next)) {
			t.Fatalf("plaintext in SQL: %q", sql)
		}
		if strings.Contains(sql, "SET PASSWORD") {
			set = sql
		}
	}
	if set != "SET PASSWORD = '"+mysqlNativeHash(string(next))+"';\n" {
		t.Fatalf("password statement %q", set)
	}
}
