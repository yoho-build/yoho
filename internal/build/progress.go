package build

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// BuildProgress is one Service build's progress, reported on every BuildKit
// vertex start or completion (not on log chunks).
type BuildProgress struct {
	Service string
	Index   int // 1-based position of Service among the Services to build
	Total   int
	Step    string // current step, e.g. "[stage-1 7/12] RUN pnpm build"
	Done    int    // steps completed (cached included)
	Steps   int    // steps seen so far
	Cached  int    // steps served from cache
}

// rawStatus is the part of BuildKit's SolveStatus (buildx --progress=rawjson)
// that Yoho reads.
type rawStatus struct {
	Vertexes []struct {
		Digest    string     `json:"digest"`
		Name      string     `json:"name"`
		Started   *time.Time `json:"started"`
		Completed *time.Time `json:"completed"`
		Cached    bool       `json:"cached"`
		Error     string     `json:"error"`
	} `json:"vertexes"`
	Logs []struct {
		Vertex string `json:"vertex"`
		Data   string `json:"data"` // base64
	} `json:"logs"`
}

type vertex struct {
	name      string
	started   bool
	completed bool
	cached    bool
	errShown  bool
	partial   string // log text after the last newline
}

// progressParser turns buildx rawjson lines on stderr into a readable text
// log on out and progress callbacks. Lines that are not JSON pass through.
type progressParser struct {
	out      io.Writer
	onChange func(step string, done, steps, cached int)
	buf      []byte
	verts    map[string]*vertex
	running  []string // digests started and not completed, oldest first
	cur      string
	sawJSON  bool
	text     bytes.Buffer // non-JSON lines, for fallback detection
}

func newProgressParser(out io.Writer, onChange func(step string, done, steps, cached int)) *progressParser {
	return &progressParser{out: out, onChange: onChange, verts: map[string]*vertex{}}
}

func (p *progressParser) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		p.line(strings.TrimRight(string(p.buf[:i]), "\r"))
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

// Close handles a final unterminated line and flushes partial log lines.
func (p *progressParser) Close() {
	if len(p.buf) > 0 {
		p.line(strings.TrimRight(string(p.buf), "\r\n"))
		p.buf = nil
	}
	for _, v := range p.verts {
		p.flush(v)
	}
}

func (p *progressParser) line(l string) {
	var st rawStatus
	if !strings.HasPrefix(l, "{") || json.Unmarshal([]byte(l), &st) != nil {
		p.text.WriteString(l + "\n")
		fmt.Fprintln(p.out, l)
		return
	}
	p.sawJSON = true
	changed := false
	for _, jv := range st.Vertexes {
		v := p.verts[jv.Digest]
		if v == nil {
			v = &vertex{}
			p.verts[jv.Digest] = v
			changed = true
		}
		if jv.Name != "" {
			v.name = jv.Name
		}
		if jv.Started != nil && !v.started {
			v.started = true
			p.running = append(p.running, jv.Digest)
			if !strings.HasPrefix(v.name, "[internal]") {
				fmt.Fprintln(p.out, "=> "+oneLine(v.name))
				p.cur = oneLine(v.name)
			}
			changed = true
		}
		if jv.Completed != nil && !v.completed {
			v.completed, v.cached = true, jv.Cached
			for i, d := range p.running {
				if d == jv.Digest {
					p.running = append(p.running[:i], p.running[i+1:]...)
					break
				}
			}
			p.flush(v)
			changed = true
		}
		if jv.Error != "" && !v.errShown {
			v.errShown = true
			fmt.Fprintf(p.out, "ERROR: %s: %s\n", oneLine(v.name), jv.Error)
		}
	}
	for _, lg := range st.Logs {
		data, err := base64.StdEncoding.DecodeString(lg.Data)
		if err != nil {
			continue
		}
		v := p.verts[lg.Vertex]
		if v == nil {
			v = &vertex{}
			p.verts[lg.Vertex] = v
		}
		text := v.partial + strings.ReplaceAll(string(data), "\r", "\n")
		lines := strings.Split(text, "\n")
		v.partial = lines[len(lines)-1]
		for _, s := range lines[:len(lines)-1] {
			if strings.TrimSpace(s) != "" {
				fmt.Fprintln(p.out, "   | "+s)
			}
		}
	}
	if changed && p.onChange != nil {
		done, steps, cached := p.counts()
		p.onChange(p.current(), done, steps, cached)
	}
}

func (p *progressParser) flush(v *vertex) {
	if strings.TrimSpace(v.partial) != "" {
		fmt.Fprintln(p.out, "   | "+v.partial)
	}
	v.partial = ""
}

// current is the newest running step, preferring non-[internal] ones, else the
// last step shown.
func (p *progressParser) current() string {
	for i := len(p.running) - 1; i >= 0; i-- {
		if n := p.verts[p.running[i]].name; !strings.HasPrefix(n, "[internal]") {
			return oneLine(n)
		}
	}
	return p.cur
}

func (p *progressParser) counts() (done, steps, cached int) {
	for _, v := range p.verts {
		if v.completed {
			done++
			if v.cached {
				cached++
			}
		}
	}
	return done, len(p.verts), cached
}

// rejectedRawJSON reports whether buildx refused --progress=rawjson (old
// buildx) rather than failing the build.
func (p *progressParser) rejectedRawJSON() bool {
	if p.sawJSON {
		return false
	}
	t := strings.ToLower(p.text.String())
	return strings.Contains(t, "rawjson") || (strings.Contains(t, "progress") && (strings.Contains(t, "invalid") || strings.Contains(t, "unknown")))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
