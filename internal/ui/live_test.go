package ui

import (
	"bytes"
	"strings"
	"testing"
)

func ttyUI(buf *bytes.Buffer) *UI {
	u := New(buf, Human, false)
	u.tty, u.cols = true, func() int { return 20 }
	return u
}

func TestLiveLineTTY(t *testing.T) {
	var buf bytes.Buffer
	u := ttyUI(&buf)
	u.LiveStart("· building web (1/2)")
	u.LiveUpdate("· building web (1/2)  [1/9] RUN a very long step name")
	buf.Reset()
	u.Info("hello")
	got := buf.String()
	// clear, the line, then redraw (truncated to the width).
	if !strings.HasPrefix(got, "\r\x1b[Khello\n\r\x1b[K") || !strings.Contains(got, "…") {
		t.Fatalf("got %q", got)
	}
	buf.Reset()
	u.LiveFinish("web 1.0s", true)
	if got := buf.String(); got != "\r\x1b[K  ✓ web 1.0s\n" {
		t.Fatalf("got %q", got)
	}
	buf.Reset()
	u.Info("after")
	if buf.String() != "after\n" {
		t.Fatalf("live line leaked: %q", buf.String())
	}
}

func TestLiveLineNoTTY(t *testing.T) {
	var buf bytes.Buffer
	u := New(&buf, Human, false)
	u.LiveStart("· building web (1/2)")
	u.LiveUpdate("· building web (1/2)  step")
	u.LiveFinish("web 1.0s", false)
	if got := buf.String(); got != "· building web (1/2)\n  ✗ web 1.0s\n" {
		t.Fatalf("got %q", got)
	}
}

func TestLiveLineJSONAndHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	u := New(&buf, JSON, false)
	u.LiveStart("x")
	u.LiveUpdate("y")
	u.LiveFinish("z", true)
	if buf.Len() != 0 {
		t.Fatalf("JSON mode printed %q", buf.String())
	}
	u.Event("build_progress", "", map[string]any{"service": "web"})
	if !strings.Contains(buf.String(), `"event":"build_progress"`) {
		t.Fatal(buf.String())
	}
}
