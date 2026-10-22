package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestShipSummaryReportsBytesAndDuration(t *testing.T) {
	detail, fields := shipSummary([]transport.Result{
		{Host: "web", Method: "load", Bytes: 412 << 20, Duration: 38 * time.Second},
		{Host: "db", Method: "skip"},
	})
	for _, want := range []string{"web via load, 412.0 MB in 38s (10.8 MB/s), db via skip"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	var b strings.Builder
	u := ui.New(&b, ui.JSON, false)
	u.Step("", "Ship 1 image(s)").DoneWith(detail, fields)
	out := b.String()
	for _, want := range []string{`"transfers":[`, `"host":"web"`, `"method":"load"`, `"bytes":432013312`, `"duration_ms":38000`, `"detail":"web via load`} {
		if !strings.Contains(out, want) {
			t.Errorf("json lacks %s:\n%s", want, out)
		}
	}
}
