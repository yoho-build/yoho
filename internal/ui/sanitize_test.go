package ui

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain line":                                "plain line",
		"tab\there":                                 "tab\there",
		"héllo — ünïcode ✓ ©°":                      "héllo — ünïcode ✓ ©°",
		"\x1b[31mred\x1b[0m":                        "red",
		"\x1b[2J\x1b[H\x1b[1;1Hcleared":             "cleared",
		"\x1b[?25l\x1b[38;2;1;2;3mx":                "x",
		"\x1b]0;pwned title\x07after":               "after",
		"\x1b]8;;https://evil.example\x1b\\link":    "link",
		"\x1bPdcs body\x1b\\ok":                     "ok",
		"\x1b_apc\x1b\\ok":                          "ok",
		"\x1b]52;c;Y2xpcGJvYXJk":                    "", // unterminated OSC 52 (clipboard)
		"\x1b(Bcharset \x1b7save\x1b8":              "charset save",
		"\x1bc reset":                               " reset",
		"a\rb\x08c\x00d\x07e\x7ff":                  "abcdef",
		"c1 \u009b31mcsi\u009d0;t\u009cend\u0085nl": "c1 csiendnl",
		"raw \x9b31m8bit":                           "raw 31m8bit",
		"trailing esc \x1b":                         "trailing esc ",
		"\x1b[31":                                   "",
		"\x1b[1\nx":                                 "x",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStepOutputStripsTerminalControl(t *testing.T) {
	for _, verbose := range []bool{true, false} { // live lines; the failure tail
		var b bytes.Buffer
		u := New(&b, Human, verbose)
		u.color = true
		s := u.Step("srv", "Drill latest")
		fmt.Fprint(s.Output(), "\x1b]0;owned\x07\x1b[2Jrows: \x1b[31m42\x1b[0m\n")
		s.Fail(errors.New("verify db failed"), "")
		out := b.String()
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[31m42"} {
			if strings.Contains(out, bad) {
				t.Errorf("verbose %v: output keeps %q:\n%q", verbose, bad, out)
			}
		}
		if !strings.Contains(out, "rows: 42") {
			t.Errorf("verbose %v: output line missing:\n%q", verbose, out)
		}
		// The UI's own colors still render.
		if !strings.Contains(out, cyan+"[srv]"+reset) || !strings.Contains(out, red+"verify db failed"+reset) {
			t.Errorf("verbose %v: UI colors missing:\n%q", verbose, out)
		}
		if verbose && !strings.Contains(out, cyan+"srv"+reset+" "+dim+"rows: 42"+reset) {
			t.Errorf("live line colors missing:\n%q", out)
		}
	}
}

func TestStepOutputJSONStripsTerminalControl(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, JSON, true)
	s := u.Step("srv", "Hook post-deploy")
	fmt.Fprint(s.Output(), "ok \x1b[1mbold\x1b[0m\n")
	s.Done()
	if strings.Contains(b.String(), `\u001b`) || !strings.Contains(b.String(), `"line":"ok bold"`) {
		t.Errorf("json:\n%s", b.String())
	}
}

func TestProgressStripsTerminalControl(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, Human, false)
	u.color = true
	fmt.Fprintln(u.Progress(), "[srv] hook \x1b]0;x\x07said \x1b[31mhi\x1b[0m")
	fmt.Fprintln(u.Progress(), "\x1b[2K") // only control: no line
	out := b.String()
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x1b[31m") || strings.Count(out, "\n") != 1 {
		t.Errorf("progress:\n%q", out)
	}
	if !strings.Contains(out, cyan+"[srv]"+reset+" hook said hi") {
		t.Errorf("progress lost host prefix or colors:\n%q", out)
	}
}
