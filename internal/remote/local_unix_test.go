package remote

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAsEnvIsMinimal(t *testing.T) {
	got := asEnv([]string{"DOCKER_HOST=tcp://evil:2375", "DOCKER_CONFIG=/tmp/x", "COMPOSE_FILE=/etc/x", "PATH=/tmp/bin", "LANG=C.UTF-8", "LC_ALL=C", "HOME=/root"}, &User{Name: "deploy", Home: "/home/deploy"})
	want := "PATH=" + SafePath + " HOME=/home/deploy USER=deploy LOGNAME=deploy LANG=C.UTF-8 LC_ALL=C"
	if strings.Join(got, " ") != want {
		t.Fatalf("env %q", got)
	}
}

// pidWriter cancels once the script printed the background child's pid.
type pidWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	pid    chan int
	cancel context.CancelFunc
	sent   bool
}

func (w *pidWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if line, _, ok := strings.Cut(w.buf.String(), "\n"); ok && !w.sent {
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			w.sent = true
			w.pid <- n
			w.cancel()
		}
	}
	return len(p), nil
}

// TestLocalGroupKillsGrandchildren is the logs -f leak: cancelling the
// request must stop the process sh started, not only sh.
func TestLocalGroupKillsGrandchildren(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &pidWriter{pid: make(chan int, 1), cancel: cancel}
	start := time.Now()
	err := (&Local{Group: true}).Run(ctx, Cmd{Script: "sleep 300 &\necho $!\nwait", Stdout: w})
	if err == nil {
		t.Fatal("want an error from the cancelled run")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("Run took %v after cancel", d)
	}
	pid := <-w.pid
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background child %d survived the cancel", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
