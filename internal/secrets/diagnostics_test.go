package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func TestDiagnosticRedactorShortAndMultilineWindow(t *testing.T) {
	var out bytes.Buffer
	w := NewDiagnosticRedactor(&out, "abc", "line-one-secret\nline-two-secret")
	// A recent-log window can start inside a multiline secret; components still
	// need masking. Short values are allowed here without changing normal output.
	for _, piece := range []string{"line-two-", "secret\n", "a", "bc"} {
		if _, err := w.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "abc") {
		t.Fatalf("leaked diagnostic value: %s", out.String())
	}
}
