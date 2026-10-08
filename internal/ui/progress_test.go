package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProgressDeltaSharedAcrossWriters(t *testing.T) {
	orig := nowFn
	t.Cleanup(func() { nowFn = orig })
	cur := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return cur }

	var b bytes.Buffer
	u := New(&b, Human, false)
	hook := u.Progress() // created before the build, like yoho deploy
	cur = cur.Add(time.Minute)
	deploy := u.Progress()
	fmt.Fprintln(deploy, "[deb] switching proxy route web")
	cur = cur.Add(100 * time.Millisecond)
	fmt.Fprintln(hook, "running hook post-deploy")

	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	if !strings.Contains(lines[0], "switching proxy route web") || !strings.Contains(lines[0], "+1m00s") {
		t.Fatalf("first line: %q", lines[0])
	}
	if !strings.Contains(lines[1], "running hook post-deploy") || !strings.Contains(lines[1], "+100ms") {
		t.Fatalf("hook line should be since the previous progress line, got %q", lines[1])
	}
}

func TestProgressDeltaResetsOnStep(t *testing.T) {
	orig := nowFn
	t.Cleanup(func() { nowFn = orig })
	cur := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return cur }

	var b bytes.Buffer
	u := New(&b, Human, false)
	cur = cur.Add(time.Minute)
	step := u.Step("deb", "Deploy v1")
	cur = cur.Add(time.Second)
	step.Done()
	cur = cur.Add(200 * time.Millisecond)
	fmt.Fprintln(u.Progress(), "[deb] preparing release")

	if !strings.Contains(b.String(), "preparing release +200ms") {
		t.Fatalf("progress after a step should not include the step, got:\n%s", b.String())
	}
}
