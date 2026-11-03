package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
)

func TestCheckRemoveConfirm(t *testing.T) {
	cases := []struct {
		f       removeFlags
		tty     bool
		json    bool
		wantErr string
	}{
		{removeFlags{}, true, false, ""},                         // asks
		{removeFlags{}, false, false, "stdin is not a terminal"}, // needs -y
		{removeFlags{yes: true}, false, false, ""},               // -y without a terminal
		{removeFlags{}, true, true, "--json requires -y"},        // --json cannot ask
		{removeFlags{yes: true, volumes: true}, true, false, ""}, // asks for the App name
		{removeFlags{yes: true, volumes: true}, false, false, "--volumes without a terminal needs --confirm shop"},
		{removeFlags{yes: true, volumes: true, confirm: "shop"}, false, true, ""},
		{removeFlags{yes: true, volumes: true, confirm: "shopx"}, false, false, "not the App name"},
		{removeFlags{yes: true, confirm: "shop"}, false, false, "--confirm applies to --volumes only"},
	}
	for i, c := range cases {
		err := checkRemoveConfirm(c.f, "shop", c.tty, c.json)
		if (err == nil) != (c.wantErr == "") || err != nil && !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("case %d: %v, want %q", i, err, c.wantErr)
		}
	}
}

func TestAskRemove(t *testing.T) {
	var out bytes.Buffer
	if err := askRemove(strings.NewReader("yes\n"), &out, removeFlags{}, "shop", 1); err != nil {
		t.Fatal(err)
	}
	if err := askRemove(strings.NewReader("y\n"), &out, removeFlags{}, "shop", 1); err == nil {
		t.Fatal("y accepted")
	}
	if err := askRemove(strings.NewReader("yes\nshop\n"), &out, removeFlags{volumes: true}, "shop", 2); err != nil {
		t.Fatal(err)
	}
	if err := askRemove(strings.NewReader("yes\nyes\n"), &out, removeFlags{volumes: true}, "shop", 2); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("wrong App name accepted: %v", err)
	}
	if err := askRemove(strings.NewReader("shop\n"), &out, removeFlags{yes: true, volumes: true}, "shop", 2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Type the App name (shop)") {
		t.Fatalf("prompt:\n%s", out.String())
	}
}

func TestCheckRunService(t *testing.T) {
	svcs := []deploy.CurrentService{{Name: "web", Proxied: true}, {Name: "db", Stateful: true}}
	if err := checkRunService(svcs, "web", false); err != nil {
		t.Fatal(err)
	}
	if err := checkRunService(svcs, "db", false); err == nil || !strings.Contains(err.Error(), "stateful") {
		t.Fatalf("stateful: %v", err)
	}
	if err := checkRunService(svcs, "db", true); err != nil {
		t.Fatal(err)
	}
	err := checkRunService(svcs, "api", false)
	if he, ok := err.(*hintError); !ok || !strings.Contains(he.hint, "web, db") {
		t.Fatalf("unknown: %v", err)
	}
}

func TestExecScripts(t *testing.T) {
	if s := composeExecScript("yoho-shop-production", termOptions{}, []string{"web", "ls"}); s != "docker compose -p 'yoho-shop-production' exec -T 'web' 'ls'" {
		t.Errorf("default %s", s)
	}
	if s := composeExecScript("yoho-shop-production", termOptions{interactive: true}, []string{"web", "sh"}); !strings.Contains(s, " exec -T ") {
		t.Errorf("-i without a pty must pass -T: %s", s)
	}
	if s := composeExecScript("yoho-shop-production", termOptions{interactive: true, tty: true}, []string{"web", "sh"}); strings.Contains(s, "-T") {
		t.Errorf("-it passes -T: %s", s)
	}
	for o, want := range map[termOptions]string{{}: "", {interactive: true}: "-i ", {tty: true}: "-it ", {interactive: true, tty: true}: "-it "} {
		if got := swarmExecFlags(o); got != want {
			t.Errorf("%+v: %q, want %q", o, got, want)
		}
	}
}
