// Package tui renders `yoho tui`: a full-screen, read-only terminal
// dashboard over a dash.Live. It uses only ANSI escapes and x/term raw mode.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/yoho-build/yoho/internal/dash"
)

// Data is what the TUI reads. *dash.Live implements it.
type Data interface {
	Snapshot(ctx context.Context, fresh bool) (*dash.Snapshot, error)
	Logs(ctx context.Context, server, name string, lines int) (string, error)
}

// Model is the TUI state. Render is a pure function of it.
type Model struct {
	Snap     *dash.Snapshot
	Err      error
	Loading  bool
	Server   int // selected Server tab
	Sel      int // selected row: 0 is the Proxy when present, then containers
	Logs     *LogView
	Color    bool
	Now      time.Time
	Interval time.Duration
}

// LogView is the logs screen of one container.
type LogView struct {
	Server, Container, Service string
	Text                       string
	Err                        error
	Loading                    bool
	Offset                     int // lines scrolled up from the bottom
}

// rows lists the selectable containers of the current Server: the Proxy
// first when present, then the App's containers.
func (m *Model) rows() []dash.Container {
	s := m.server()
	if s == nil {
		return nil
	}
	var out []dash.Container
	if s.Proxy != nil {
		out = append(out, *s.Proxy)
	}
	return append(out, s.Containers...)
}

func (m *Model) server() *dash.Server {
	if m.Snap == nil || len(m.Snap.Servers) == 0 {
		return nil
	}
	if m.Server >= len(m.Snap.Servers) {
		m.Server = len(m.Snap.Servers) - 1
	}
	if m.Server < 0 {
		m.Server = 0
	}
	return &m.Snap.Servers[m.Server]
}

func (m *Model) clamp() {
	n := len(m.rows())
	if m.Sel >= n {
		m.Sel = n - 1
	}
	if m.Sel < 0 {
		m.Sel = 0
	}
}

// Key is a decoded key press.
type Key string

const (
	KeyUp       Key = "up"
	KeyDown     Key = "down"
	KeyLeft     Key = "left"
	KeyRight    Key = "right"
	KeyTab      Key = "tab"
	KeyBackTab  Key = "backtab"
	KeyEnter    Key = "enter"
	KeyEsc      Key = "esc"
	KeyCtrlC    Key = "ctrl-c"
	KeyPageUp   Key = "pgup"
	KeyPageDown Key = "pgdn"
)

// ParseKeys decodes a raw-mode read into key presses.
func ParseKeys(b []byte) []Key {
	var keys []Key
	for len(b) > 0 {
		switch {
		case b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O'):
			seq := map[byte]Key{'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft, 'Z': KeyBackTab}
			if k, ok := seq[b[2]]; ok {
				keys = append(keys, k)
				b = b[3:]
				continue
			}
			if len(b) >= 4 && b[3] == '~' && (b[2] == '5' || b[2] == '6') {
				keys = append(keys, map[byte]Key{'5': KeyPageUp, '6': KeyPageDown}[b[2]])
				b = b[4:]
				continue
			}
			// Unknown sequence: drop it up to its final byte.
			i := 2
			for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
				i++
			}
			b = b[min(i+1, len(b)):]
		case b[0] == 0x1b:
			keys = append(keys, KeyEsc)
			b = b[1:]
		case b[0] == 3:
			keys = append(keys, KeyCtrlC)
			b = b[1:]
		case b[0] == '\t':
			keys = append(keys, KeyTab)
			b = b[1:]
		case b[0] == '\r' || b[0] == '\n':
			keys = append(keys, KeyEnter)
			b = b[1:]
		case b[0] == 0x7f || b[0] == 8:
			keys = append(keys, KeyEsc)
			b = b[1:]
		default:
			r, n := utf8.DecodeRune(b)
			keys = append(keys, Key(string(r)))
			b = b[n:]
		}
	}
	return keys
}

// Action is what a key asks the run loop to do besides re-rendering.
type Action int

const (
	ActNone Action = iota
	ActQuit
	ActRefresh
	ActLogs
)

// Update applies a key to the model.
func (m *Model) Update(k Key, height int) Action {
	if k == KeyCtrlC || k == "q" && m.Logs == nil {
		return ActQuit
	}
	if m.Logs != nil {
		page := max(1, height-6)
		switch k {
		case KeyEsc, "q", KeyLeft, "h":
			m.Logs = nil
		case KeyUp, "k":
			m.Logs.Offset++
		case KeyDown, "j":
			m.Logs.Offset = max(0, m.Logs.Offset-1)
		case KeyPageUp, "b":
			m.Logs.Offset += page
		case KeyPageDown, " ", "f":
			m.Logs.Offset = max(0, m.Logs.Offset-page)
		case "G":
			m.Logs.Offset = 0
		case "g":
			m.Logs.Offset = strings.Count(m.Logs.Text, "\n") + 1
		case "r":
			return ActLogs
		}
		return ActNone
	}
	n := 0
	if m.Snap != nil {
		n = len(m.Snap.Servers)
	}
	switch k {
	case "r":
		return ActRefresh
	case KeyTab, KeyRight, "l":
		if n > 0 {
			m.Server = (m.Server + 1) % n
			m.Sel = 0
		}
	case KeyBackTab, KeyLeft, "h":
		if n > 0 {
			m.Server = (m.Server + n - 1) % n
			m.Sel = 0
		}
	case KeyUp, "k":
		m.Sel--
	case KeyDown, "j":
		m.Sel++
	case KeyEnter:
		rows := m.rows()
		m.clamp()
		if len(rows) == 0 {
			return ActNone
		}
		c := rows[m.Sel]
		m.Logs = &LogView{Server: m.server().Name, Container: c.Name, Service: c.Service, Loading: true}
		return ActLogs
	}
	m.clamp()
	return ActNone
}

// Options configure Run.
type Options struct {
	Interval time.Duration
	Lines    int // log lines to fetch
}

// Run takes over the terminal until q or Ctrl-C. in must be a terminal.
func Run(ctx context.Context, data Data, in *os.File, out io.Writer, opt Options) error {
	fd := int(in.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("yoho tui needs a terminal: %w", err)
	}
	defer term.Restore(fd, state)
	io.WriteString(out, "\x1b[?1049h\x1b[?25l")
	defer io.WriteString(out, "\x1b[2J\x1b[H\x1b[?25h\x1b[?1049l")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			b := append([]byte(nil), buf[:n]...)
			select {
			case keys <- b:
			case <-ctx.Done():
				return
			}
		}
	}()

	type snapResult struct {
		snap *dash.Snapshot
		err  error
	}
	type logResult struct {
		key  string
		text string
		err  error
	}
	snaps := make(chan snapResult, 1)
	logs := make(chan logResult, 1)
	collecting := false
	collect := func(fresh bool) {
		if collecting {
			return
		}
		collecting = true
		go func() {
			s, err := data.Snapshot(ctx, fresh)
			snaps <- snapResult{s, err}
		}()
	}
	fetchLogs := func(lv *LogView) {
		lv.Loading = true
		key := lv.Server + "/" + lv.Container
		go func() {
			t, err := data.Logs(ctx, lv.Server, lv.Container, opt.Lines)
			select {
			case logs <- logResult{key, t, err}:
			case <-ctx.Done():
			}
		}()
	}

	m := &Model{Color: os.Getenv("NO_COLOR") == "", Interval: opt.Interval, Loading: true}
	ticker := time.NewTicker(opt.Interval)
	defer ticker.Stop()
	draw := func() {
		w, h, err := term.GetSize(fd)
		if err != nil || w <= 0 || h <= 0 {
			w, h = 100, 30
		}
		m.Now = time.Now()
		io.WriteString(out, "\x1b[H"+Render(m, w, h))
	}
	collect(false)
	draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			_, h, _ := term.GetSize(fd)
			for _, k := range ParseKeys(b) {
				switch m.Update(k, h) {
				case ActQuit:
					return nil
				case ActRefresh:
					m.Loading = true
					collect(true)
				case ActLogs:
					fetchLogs(m.Logs)
				}
			}
			draw()
		case r := <-snaps:
			collecting = false
			m.Loading = false
			if r.err != nil {
				if ctx.Err() != nil {
					return nil
				}
				m.Err = r.err
			} else {
				m.Snap, m.Err = r.snap, nil
				m.clamp()
			}
			draw()
		case r := <-logs:
			if m.Logs != nil && m.Logs.Server+"/"+m.Logs.Container == r.key {
				m.Logs.Text, m.Logs.Err, m.Logs.Loading = r.text, r.err, false
			}
			draw()
		case <-ticker.C:
			if m.Logs == nil {
				collect(false)
			}
			draw()
		}
	}
}
