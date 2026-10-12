package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestError(t *testing.T) {
	var b bytes.Buffer
	New(&b, Human, false).Error(errors.New("bad Service"), "name one")
	if got := b.String(); got != "✗ bad Service\n  hint: name one\n" {
		t.Errorf("human: %q", got)
	}
	b.Reset()
	New(&b, JSON, false).Error(errors.New("bad Service"), "name one")
	var ev map[string]any
	if err := json.Unmarshal(b.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["event"] != "error" || ev["error"] != "bad Service" || ev["hint"] != "name one" {
		t.Errorf("json: %v", ev)
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Errorf("json lines: %q", b.String())
	}
}
