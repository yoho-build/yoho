package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// minRedactLen skips very short values, which would shred ordinary output.
const minRedactLen = 4

type pattern struct {
	text  []byte
	label []byte
}

// Redactor is an io.Writer that replaces secret values, and their base64,
// URL-encoded and JSON-escaped forms, before passing output on. A value split
// across Write calls is still caught: the longest tail that could begin a
// value is held back until more data arrives or Flush is called.
type Redactor struct {
	mu     sync.Mutex
	w      io.Writer
	pats   []pattern // longest first
	maxLen int
	buf    []byte
}

// NewRedactor redacts values as "***".
func NewRedactor(w io.Writer, values ...string) *Redactor {
	m := map[string]string{}
	for _, v := range values {
		m[v] = "***"
	}
	return newRedactor(w, m)
}

// Redactor redacts every value in the Store as [REDACTED:KEY].
func (s *Store) Redactor(w io.Writer) *Redactor {
	m := map[string]string{}
	for _, k := range s.Keys() { // sorted: the first key wins for shared values
		if _, ok := m[s.values[k]]; !ok {
			m[s.values[k]] = "[REDACTED:" + k + "]"
		}
	}
	return newRedactor(w, m)
}

func newRedactor(w io.Writer, valueLabels map[string]string) *Redactor {
	r := &Redactor{w: w}
	seen := map[string]bool{}
	for v, label := range valueLabels {
		if len(v) < minRedactLen {
			continue
		}
		for _, variant := range variants(v) {
			if seen[variant] {
				continue
			}
			seen[variant] = true
			r.pats = append(r.pats, pattern{[]byte(variant), []byte(label)})
			r.maxLen = max(r.maxLen, len(variant))
		}
	}
	sort.Slice(r.pats, func(i, j int) bool {
		if len(r.pats[i].text) != len(r.pats[j].text) {
			return len(r.pats[i].text) > len(r.pats[j].text)
		}
		return bytes.Compare(r.pats[i].text, r.pats[j].text) < 0
	})
	return r
}

func variants(v string) []string {
	out := []string{
		v,
		base64.StdEncoding.EncodeToString([]byte(v)),
		base64.RawStdEncoding.EncodeToString([]byte(v)),
		base64.URLEncoding.EncodeToString([]byte(v)),
		base64.RawURLEncoding.EncodeToString([]byte(v)),
		url.QueryEscape(v),
		url.PathEscape(v),
	}
	if j, err := json.Marshal(v); err == nil {
		out = append(out, string(j[1:len(j)-1]))
	}
	return out
}

// Write redacts p and writes what is safe to emit. It reports len(p) on
// success so callers such as io.Copy don't see a short write.
func (r *Redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pats) == 0 {
		return r.w.Write(p)
	}
	r.buf = append(r.buf, p...)
	out, rest := r.process(r.buf, false)
	r.buf = append(r.buf[:0], rest...)
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes any held-back tail.
func (r *Redactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out, _ := r.process(r.buf, true)
	r.buf = r.buf[:0]
	if len(out) == 0 {
		return nil
	}
	_, err := r.w.Write(out)
	return err
}

// Close flushes. It does not close the underlying writer.
func (r *Redactor) Close() error { return r.Flush() }

// process returns redacted output and, unless final, the tail to hold back.
func (r *Redactor) process(data []byte, final bool) (out, rest []byte) {
	i := 0
	for i < len(data) {
		j, pat := r.find(data, i)
		if pat == nil {
			break
		}
		// A longer pattern might still match at j once more data arrives.
		if !final && r.couldExtend(data[j:], len(pat.text)) {
			out = append(out, data[i:j]...)
			return out, data[j:]
		}
		out = append(out, data[i:j]...)
		out = append(out, pat.label...)
		i = j + len(pat.text)
	}
	if final {
		return append(out, data[i:]...), nil
	}
	hold := r.holdBack(data[i:])
	cut := len(data) - hold
	return append(out, data[i:cut]...), data[cut:]
}

// find returns the leftmost match at or after i, preferring the longest.
func (r *Redactor) find(data []byte, i int) (int, *pattern) {
	best, bestIdx := (*pattern)(nil), -1
	for k := range r.pats {
		p := &r.pats[k]
		idx := bytes.Index(data[i:], p.text)
		if idx < 0 {
			continue
		}
		if bestIdx < 0 || idx < bestIdx { // pats are longest first, so ties keep the longest
			best, bestIdx = p, idx
		}
	}
	if best == nil {
		return -1, nil
	}
	return i + bestIdx, best
}

func (r *Redactor) couldExtend(tail []byte, matched int) bool {
	for _, p := range r.pats {
		if len(p.text) > matched && len(tail) < len(p.text) && bytes.HasPrefix(p.text, tail) {
			return true
		}
	}
	return false
}

// holdBack is the length of the longest suffix of data that is a proper
// prefix of some pattern.
func (r *Redactor) holdBack(data []byte) int {
	for k := min(len(data), r.maxLen-1); k > 0; k-- {
		suffix := data[len(data)-k:]
		for _, p := range r.pats {
			if len(p.text) > k && bytes.HasPrefix(p.text, suffix) {
				return k
			}
		}
	}
	return 0
}

// redactString replaces known values in s with "***" (used for stderr in errors).
func redactString(s string, values []string) string {
	vals := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= minRedactLen {
			vals = append(vals, variants(v)...)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
