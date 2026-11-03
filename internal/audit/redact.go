package audit

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yoho-build/yoho/internal/secrets"
)

// Redactor masks secret values in text recorded in the audit log: first the
// known secret values (the secrets redactor, with their base64, URL and JSON
// forms), then values that look like secrets by name: NAME=value where NAME
// says password, token, secret, key or credential, --password VALUE style
// flags, Authorization headers and user:password@ in URLs. The second pass
// covers values Yoho does not know (a token typed on the command line).
type Redactor struct {
	values []string
	// known masks the values of a loaded secrets Store (Store.RedactString).
	known func(string) string
}

// NewRedactor returns a Redactor for the known secret values.
func NewRedactor(values ...string) *Redactor { return &Redactor{values: values} }

// sensitiveName matches names of variables and flags that hold secrets.
const sensitiveName = `(?:pass(?:word|wd|phrase)?|pwd|secret|token|api[-_]?key|access[-_]?key|private[-_]?key|[-_]key|key[-_]|credential|cred|auth|bearer|signature|dsn|cookie)`

var (
	assignRe = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*` + sensitiveName + `[A-Za-z0-9_.-]*)=("[^"]*"|'[^']*'|[^\s"']+)`)
	flagRe   = regexp.MustCompile(`(?i)^--?[a-z0-9_.-]*` + sensitiveName + `[a-z0-9_.-]*$`)
	urlRe    = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@'"]*):[^\s/@'"]+@`)
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{6,}`)
)

// WithStore also masks every value of a loaded secrets Store, through its
// RedactString.
func (r *Redactor) WithStore(redact func(string) string) *Redactor {
	r.known = redact
	return r
}

// String redacts s.
func (r *Redactor) String(s string) string {
	if r != nil && len(r.values) > 0 {
		var b bytes.Buffer
		w := secrets.NewRedactor(&b, r.values...)
		_, _ = w.Write([]byte(s))
		_ = w.Flush()
		s = b.String()
	}
	if r != nil && r.known != nil {
		s = r.known(s)
	}
	s = assignRe.ReplaceAllString(s, "${1}=***")
	s = urlRe.ReplaceAllString(s, "${1}:***@")
	return bearerRe.ReplaceAllString(s, "${1} ***")
}

// Argv redacts an argv: every argument as text, and the argument after a
// flag that names a secret (--password hunter2).
func (r *Redactor) Argv(argv []string) []string {
	out := make([]string, len(argv))
	mask := false
	for i, a := range argv {
		if mask {
			out[i] = "***"
			mask = false
			continue
		}
		out[i] = r.String(a)
		mask = flagRe.MatchString(a) && !strings.Contains(a, "=")
	}
	return out
}
