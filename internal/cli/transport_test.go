package cli

import (
	"bytes"
	"encoding/json"
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
