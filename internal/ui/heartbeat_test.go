package ui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestHeartbeatPrintsWhenSilent(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = old }()
	var buf syncBuf
	u := New(&buf, Human, false)
	s := u.Step("", "Build")
	fmt.Fprintln(s.Output(), "#12 exporting layers")
	time.Sleep(120 * time.Millisecond)
	s.Done()
	out := buf.String()
	if !strings.Contains(out, "… still running (last: #12 exporting layers)") {
		t.Fatalf("no heartbeat:\n%s", out)
	}
	n := strings.Count(out, "still running")
	time.Sleep(80 * time.Millisecond)
	if strings.Count(buf.String(), "still running") != n {
		t.Fatal("heartbeat continued after Done")
	}
}

func TestHeartbeatSilentInVerboseAndJSON(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	defer func() { heartbeatInterval = old }()
	for _, tc := range []struct {
		mode Mode
		verb bool
	}{{Human, true}, {JSON, false}} {
		var buf syncBuf
		u := New(&buf, tc.mode, tc.verb)
		s := u.Step("", "Build")
		time.Sleep(60 * time.Millisecond)
		s.Done()
		if strings.Contains(buf.String(), "still running") {
			t.Fatalf("mode %v verbose %v printed heartbeat:\n%s", tc.mode, tc.verb, buf.String())
		}
	}
}

func TestHeartbeatSkippedWhenOutputRecent(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 200 * time.Millisecond
	defer func() { heartbeatInterval = old }()
	var buf syncBuf
	u := New(&buf, Human, false)
	s := u.Step("", "Build")
	time.Sleep(120 * time.Millisecond)
	u.Info("progress") // resets the silence clock
	time.Sleep(120 * time.Millisecond)
	s.Done()
	if strings.Contains(buf.String(), "still running") {
		t.Fatalf("unexpected heartbeat:\n%s", buf.String())
	}
}
