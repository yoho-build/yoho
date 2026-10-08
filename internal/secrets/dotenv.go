package secrets

import (
	"fmt"
	"strings"
)

// The .yoho/secrets files use Kamal's dotenv dialect:
//
//	# comment
//	export KEY=value          # "export " is optional; inline comments need a space before #
//	KEY='literal $NOT_EXPANDED'
//	KEY="line1\nline2 $OTHER ${OTHER}"   (may span several lines)
//	KEY=$(op read op://Vault/Item/field)
//
// $(...) is command substitution run by `sh -c` on the operator's machine,
// so these files are trusted code. Parsing never runs anything; commands run
// only in Load.

type partKind int

const (
	litPart partKind = iota
	varPart
	cmdPart
)

type part struct {
	kind partKind
	s    string // literal text, variable name, or shell command
}

type fileEntry struct {
	key   string
	file  string // display name, e.g. .yoho/secrets
	line  int
	parts []part
}

type dotenvParser struct {
	file string
	src  string
	pos  int
	line int
}

func parseDotenv(file string, data []byte) ([]fileEntry, error) {
	p := &dotenvParser{file: file, src: string(data), line: 1}
	var out []fileEntry
	for {
		p.skipBlank()
		if p.eof() {
			return out, nil
		}
		if p.peek() == '#' {
			p.skipLine()
			continue
		}
		line := p.line
		key := p.ident()
		if key == "export" && (p.peek() == ' ' || p.peek() == '\t') {
			p.skipSpaces()
			key = p.ident()
		}
		if key == "" {
			return nil, p.errf("expected a variable name")
		}
		p.skipSpaces()
		if p.peek() != '=' {
			return nil, p.errf("expected '=' after %s", key)
		}
		p.pos++
		p.skipSpaces()
		parts, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, fileEntry{key: key, file: file, line: line, parts: parts})
	}
}

func (p *dotenvParser) errf(format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", p.file, p.line, fmt.Sprintf(format, args...))
}

func (p *dotenvParser) eof() bool { return p.pos >= len(p.src) }

func (p *dotenvParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *dotenvParser) next() byte {
	c := p.src[p.pos]
	p.pos++
	if c == '\n' {
		p.line++
	}
	return c
}

func (p *dotenvParser) skipBlank() {
	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.next()
		default:
			return
		}
	}
}

func (p *dotenvParser) skipSpaces() {
	for p.peek() == ' ' || p.peek() == '\t' {
		p.pos++
	}
}

func (p *dotenvParser) skipLine() {
	for !p.eof() && p.next() != '\n' {
	}
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func (p *dotenvParser) ident() string {
	if !isNameStart(p.peek()) {
		return ""
	}
	start := p.pos
	for !p.eof() && isNameChar(p.peek()) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// endOfLine accepts trailing spaces and an optional comment after a quoted value.
func (p *dotenvParser) endOfLine() error {
	p.skipSpaces()
	switch p.peek() {
	case 0, '\n', '\r':
		return nil
	case '#':
		p.skipLine()
		return nil
	}
	return p.errf("unexpected %q after closing quote", p.peek())
}

func (p *dotenvParser) value() ([]part, error) {
	switch p.peek() {
	case '\'':
		p.next()
		start, line := p.pos, p.line
		for !p.eof() && p.peek() != '\'' {
			p.next()
		}
		if p.eof() {
			p.line = line
			return nil, p.errf("unterminated single quote")
		}
		lit := p.src[start:p.pos]
		p.next()
		return []part{{litPart, lit}}, p.endOfLine()
	case '"':
		p.next()
		parts, err := p.doubleQuoted()
		if err != nil {
			return nil, err
		}
		return parts, p.endOfLine()
	}
	return p.unquoted()
}

func addLit(parts []part, s string) []part {
	if s == "" {
		return parts
	}
	if n := len(parts); n > 0 && parts[n-1].kind == litPart {
		parts[n-1].s += s
		return parts
	}
	return append(parts, part{litPart, s})
}

func (p *dotenvParser) doubleQuoted() ([]part, error) {
	var parts []part
	line := p.line
	for {
		if p.eof() {
			p.line = line
			return nil, p.errf("unterminated double quote")
		}
		c := p.peek()
		switch c {
		case '"':
			p.next()
			return parts, nil
		case '\\':
			p.next()
			if p.eof() {
				continue
			}
			e := p.next()
			switch e {
			case 'n':
				parts = addLit(parts, "\n")
			case 't':
				parts = addLit(parts, "\t")
			case 'r':
				parts = addLit(parts, "\r")
			case '"', '\\', '$':
				parts = addLit(parts, string(e))
			default:
				parts = addLit(parts, "\\"+string(e))
			}
		case '$':
			pt, err := p.dollar()
			if err != nil {
				return nil, err
			}
			if pt.kind == litPart {
				parts = addLit(parts, pt.s)
			} else {
				parts = append(parts, pt)
			}
		default:
			parts = addLit(parts, string(p.next()))
		}
	}
}

func (p *dotenvParser) unquoted() ([]part, error) {
	var parts []part
	prevSpace := true // a # right after = starts a comment
	for !p.eof() {
		c := p.peek()
		if c == '\n' {
			break
		}
		if c == '#' && prevSpace {
			p.skipLine()
			break
		}
		if c == '$' {
			pt, err := p.dollar()
			if err != nil {
				return nil, err
			}
			if pt.kind == litPart {
				parts = addLit(parts, pt.s)
			} else {
				parts = append(parts, pt)
			}
			prevSpace = false
			continue
		}
		p.next()
		prevSpace = c == ' ' || c == '\t'
		parts = addLit(parts, string(c))
	}
	if n := len(parts); n > 0 && parts[n-1].kind == litPart {
		parts[n-1].s = strings.TrimRight(parts[n-1].s, " \t\r")
		if parts[n-1].s == "" {
			parts = parts[:n-1]
		}
	}
	return parts, nil
}

// dollar parses $(cmd), ${NAME}, $NAME, or a lone $ at p.pos.
func (p *dotenvParser) dollar() (part, error) {
	p.next() // $
	switch c := p.peek(); {
	case c == '(':
		p.next()
		start, line := p.pos, p.line
		depth := 1
		for {
			if p.eof() {
				p.line = line
				return part{}, p.errf("unterminated $(")
			}
			switch p.next() {
			case '\\':
				if !p.eof() {
					p.next()
				}
			case '\'':
				for !p.eof() && p.next() != '\'' {
				}
			case '"':
				for !p.eof() {
					d := p.next()
					if d == '\\' && !p.eof() {
						p.next()
					} else if d == '"' {
						break
					}
				}
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return part{cmdPart, p.src[start : p.pos-1]}, nil
				}
			}
		}
	case c == '{':
		end := strings.IndexByte(p.src[p.pos:], '}')
		if end > 1 {
			name := p.src[p.pos+1 : p.pos+end]
			if validName(name) {
				p.pos += end + 1
				return part{varPart, name}, nil
			}
		}
		return part{litPart, "$"}, nil
	case isNameStart(c):
		return part{varPart, p.ident()}, nil
	}
	return part{litPart, "$"}, nil
}

func validName(s string) bool {
	if s == "" || !isNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i]) {
			return false
		}
	}
	return true
}
