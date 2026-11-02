package cli

import (
	"fmt"
	"sync"
	"time"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/ui"
)

// progressEvery throttles build_progress events per Service.
const progressEvery = 500 * time.Millisecond

// buildLive reports each Service build to the UI: a live line on a terminal,
// build_* events with --json.
type buildLive struct {
	u      *ui.UI
	redact func(string) string

	mu       sync.Mutex
	svc      string
	index    int
	total    int
	start    time.Time
	last     build.BuildProgress
	pending  bool // last progress not yet emitted as an event
	emitted  time.Time
	stop     chan struct{}
	finished chan struct{}
}

func (b *buildLive) label() string {
	return fmt.Sprintf("· building %s (%d/%d)", b.svc, b.index, b.total)
}

// render is the live line text. Caller holds b.mu.
func (b *buildLive) render() string {
	t := b.label()
	if b.last.Step != "" {
		t += "  " + b.redact(b.last.Step)
	}
	return t + "  " + fmtLive(time.Since(b.start))
}

func fmtLive(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// started is Options.OnBuild.
func (b *buildLive) started(svc string, i, n int) {
	b.mu.Lock()
	b.svc, b.index, b.total, b.start = svc, i, n, time.Now()
	b.last, b.pending, b.emitted = build.BuildProgress{}, false, time.Time{}
	b.stop, b.finished = make(chan struct{}), make(chan struct{})
	stop, finished := b.stop, b.finished
	b.mu.Unlock()
	b.u.Event("build_service", "", map[string]any{"service": svc, "index": i, "total": n})
	b.u.LiveStart("  " + b.label())
	go func() { // keep the elapsed time moving between steps
		defer close(finished)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.mu.Lock()
				text := b.render()
				b.mu.Unlock()
				b.u.LiveUpdate("  " + text)
			}
		}
	}()
}

// progress is Options.Progress.
func (b *buildLive) progress(p build.BuildProgress) {
	b.mu.Lock()
	b.last, b.pending = p, true
	text := b.render()
	var ev map[string]any
	if time.Since(b.emitted) >= progressEvery {
		ev = b.eventLocked()
	}
	b.mu.Unlock()
	b.u.LiveUpdate("  " + text)
	if ev != nil {
		b.u.Event("build_progress", "", ev)
	}
}

// eventLocked returns the build_progress fields and marks them emitted.
func (b *buildLive) eventLocked() map[string]any {
	b.pending, b.emitted = false, time.Now()
	p := b.last
	return map[string]any{"service": b.svc, "index": b.index, "total": b.total,
		"step": b.redact(p.Step), "done": p.Done, "steps_total": p.Steps, "cached": p.Cached}
}

// built is Options.OnBuilt.
func (b *buildLive) built(svc string, err error, d time.Duration) {
	b.mu.Lock()
	stop, finished := b.stop, b.finished
	b.mu.Unlock()
	if stop != nil {
		close(stop)
		<-finished
	}
	b.mu.Lock()
	b.stop = nil
	var ev map[string]any
	if b.pending {
		ev = b.eventLocked()
	}
	cached := b.last.Cached
	b.mu.Unlock()
	if ev != nil {
		b.u.Event("build_progress", "", ev)
	}
	text := fmt.Sprintf("%s %s", svc, fmtLive(d))
	if cached > 0 {
		text += fmt.Sprintf(" (%d cached)", cached)
	}
	b.u.Event("build_service_done", "", map[string]any{"service": svc, "duration_ms": d.Milliseconds(), "cached": cached, "ok": err == nil})
	b.u.LiveFinish(text, err == nil)
}
