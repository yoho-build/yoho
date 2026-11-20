package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/yoho-build/yoho/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestTransportDecisionOutput(t *testing.T) {
	for _, mode := range []ui.Mode{ui.Human, ui.JSON} {
		var out bytes.Buffer
		a := &app{ui: ui.New(&out, mode, false)}
		d := transport.Fallback("store")
		a.transportDecision("server", d)
		if mode == ui.JSON {
			var event map[string]any
			if err := json.Unmarshal(out.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"event": "transport", "mode": "load", "host": "server", "reason": d.Reason, "hint": d.Hint} {
				if event[k] != v {
					t.Fatalf("%s: %v want %s", k, event[k], v)
				}
			}
		} else if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), d.Reason) || !strings.Contains(out.String(), d.Hint) {
			t.Fatal(out.String())
		}
	}
}

func TestTransportDecisionWithoutHint(t *testing.T) {
	var out bytes.Buffer
	a := &app{ui: ui.New(&out, ui.Human, false)}
	a.transportDecision("server", transport.Decision{Mode: "load", Reason: "explicit transport"})
	if strings.Contains(out.String(), "hint:") || !strings.Contains(out.String(), "server: load: explicit transport") {
		t.Fatal(out.String())
	}
}

func TestScopedShipRefusesPusshBeforeCommands(t *testing.T) {
	var out bytes.Buffer
	a := &app{cfg: &config.Config{Transport: config.Transport{Mode: "pussh"}}, ui: ui.New(&out, ui.JSON, false)}
	_, _, err := a.scopedShip(context.Background(), nil, []string{"x:1"}, "linux/amd64")
	if err == nil || !strings.Contains(err.Error(), "not available through the Gate") {
		t.Fatalf("%v", err)
	}
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
}

func TestDirectBuilderFatalErrorDoesNotReportTransport(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	a := &app{cfg: &config.Config{Builder: config.Builder{Location: "remote", Remote: "build@example.invalid", Direct: config.DirectOn}, Transport: config.Transport{Mode: "pussh"}}, ui: ui.New(&out, ui.JSON, false)}
	h, err := a.directBuilder(context.Background(), nil, []string{"linux/amd64"}, nil)
	if h != nil || err == nil || !strings.Contains(err.Error(), "transport.mode pussh") {
		t.Fatalf("%v %v", h, err)
	}
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
}
