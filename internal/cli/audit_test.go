package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/audit"
)

func TestAuditArgsRedactExecArgvAndKeepKeyNames(t *testing.T) {
	red := audit.NewRedactor("sup3r-secret-value")
	c := &cobra.Command{Use: "exec"}
	c.Flags().BoolP("tty", "t", false, "")
	c.Flags().StringSlice("generated", nil, "")
	c.Flags().String("config", "", "")
	if err := c.ParseFlags([]string{"-t", "--generated", "DB_PASSWORD,API_KEY", "--config", "x.yml"}); err != nil {
		t.Fatal(err)
	}
	got := auditArgs(c, "app exec", []string{"web", "env", "TOKEN=sup3r-secret-value", "echo", "sup3r-secret-value"}, red)
	if got["tty"] != true || got["service"] != "web" || got["config"] != nil {
		t.Fatalf("%v", got)
	}
	if g := got["generated"].([]string); len(g) != 2 || g[0] != "DB_PASSWORD" {
		t.Fatalf("key names lost: %v", g)
	}
	if s := strings.Join(got["argv"].([]string), " "); strings.Contains(s, "sup3r-secret-value") {
		t.Fatalf("argv leaks: %s", s)
	}
}

func TestAuditCommandsStillRunWithoutAYohoFile(t *testing.T) {
	t.Chdir(t.TempDir())
	root := &cobra.Command{Use: "yoho", SilenceUsage: true, SilenceErrors: true}
	var ran []string
	mk := func(parent *cobra.Command, use string) {
		parent.AddCommand(&cobra.Command{Use: use, RunE: func(*cobra.Command, []string) error { ran = append(ran, use); return nil }})
	}
	app := &cobra.Command{Use: "app"}
	root.AddCommand(app)
	mk(root, "deploy")
	mk(root, "plan")
	mk(app, "exec")
	mk(app, "ps")
	auditWrap(&globals{}, root)
	// Audited commands try to start a run, which needs a Yoho file: with none
	// the audit is skipped and the command itself still runs.
	for _, args := range [][]string{{"deploy"}, {"plan"}, {"app", "exec"}, {"app", "ps"}} {
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if strings.Join(ran, ",") != "deploy,plan,exec,ps" {
		t.Fatalf("ran %v", ran)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"36h": now.Add(-36 * time.Hour), "7d": now.Add(-7 * 24 * time.Hour), "2w": now.Add(-14 * 24 * time.Hour),
		"2026-11-01T00:00:00Z": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if d, err := parseSince("2026-11-01", now); err != nil || d.Day() != 1 {
		t.Errorf("date: %v %v", d, err)
	}
	for _, bad := range []string{"", "soon", "-3h"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
