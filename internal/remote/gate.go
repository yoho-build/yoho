package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// GateProtocol is the version of the Gate wire format. The Gate refuses
// requests with another version.
const GateProtocol = 1

// GateRequest is the first line a Gate client sends: one JSON object ending
// in '\n'. For deploy, an image archive may follow on the same stream as
// chunks (see GatePayload); stdin then stays open so the Gate can tell when
// the client goes away.
type GateRequest struct {
	Protocol    int             `json:"protocol"`
	App         string          `json:"app"`
	Destination string          `json:"destination"`
	Verb        string          `json:"verb"`
	Args        json.RawMessage `json:"args,omitempty"`
}

// GateFrame is one line the Gate writes on stdout: progress text, the final
// result, or the final error. Exactly one of Result or Error ends a call.
type GateFrame struct {
	Out    string          `json:"out,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *GateError      `json:"error,omitempty"`
}

// GateError is a refused or failed Gate request.
type GateError struct {
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Code: denied, invalid, failed, no-containers, unsupported.
	Code string `json:"code,omitempty"`
}

func (e *GateError) Error() string { return e.Message }

// MaxGateRequest bounds the request line (a compiled compose file fits easily).
const MaxGateRequest = 16 << 20

// Gate is a Server reached through `yoho gate`: a restricted key of the
// yoho-gate user whose forced command reads one GateRequest. Unlike Host it
// cannot run scripts; it calls fixed verbs.
type Gate struct {
	ssh         *SSH
	App         string
	Destination string
}

// NewGate wraps s (connected as the Gate user) for one App Destination.
func NewGate(s *SSH, app, destination string) *Gate {
	return &Gate{ssh: s, App: app, Destination: destination}
}

// Name is the Server name.
func (g *Gate) Name() string { return g.ssh.Name() }

// Target is the ssh target of the Gate user.
func (g *Gate) Target() string { return g.ssh.Target() }

// Close releases the ssh connection.
func (g *Gate) Close() error { return g.ssh.Close() }

// Call sends verb with args, streams payload after the request line (nil:
// none), writes progress text to out and decodes the result into result
// (nil: ignored). A refusal or failure reported by the Gate is a *GateError.
func (g *Gate) Call(ctx context.Context, verb string, args any, payload io.Reader, out io.Writer, result any) error {
	line, err := EncodeGateRequest(g.App, g.Destination, verb, args)
	if err != nil {
		return err
	}
	if out == nil {
		out = io.Discard
	}
	// The Gate stops open-ended verbs (logs -f) when stdin closes, so stdin
	// stays open until the call ends: request line, payload, then wait. It
	// is an OS pipe, not an io.Pipe: ssh gets the descriptor itself, so
	// waiting for ssh never waits for this writer (which would deadlock
	// when ssh exits first, e.g. on a refused key).
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	dec := &frameDecoder{out: out, final: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer pw.Close()
		if _, err := pw.Write(line); err != nil {
			return
		}
		if payload != nil {
			if err := writeChunks(pw, payload); err != nil {
				return
			}
		}
		select {
		case <-done:
		case <-ctx.Done():
		case <-dec.final:
		}
	}()
	argv := append(g.ssh.SSHArgs(), "-T", "--", g.ssh.Destination(), "yoho-gate "+verb)
	runErr := g.ssh.exec(ctx, "yoho gate "+verb, argv, pr, dec, nil)
	close(done)
	pr.Close() // a writer blocked on a full pipe now fails instead of hanging
	if ferr := dec.finish(); ferr != nil {
		return ferr
	}
	if dec.gateErr != nil {
		return dec.gateErr
	}
	if dec.result != nil {
		if result != nil {
			if err := json.Unmarshal(dec.result, result); err != nil {
				return fmt.Errorf("gate %s: bad result: %w", verb, err)
			}
		}
		return nil
	}
	if runErr != nil {
		var ee *ExitError
		if errors.As(runErr, &ee) && ee.Code == 255 {
			return fmt.Errorf("gate on %s did not answer (exit %d): %s\nhint: check that `yoho access key add` authorized your key and `yoho access install` ran on the Server", g.Name(), ee.Code, strings.TrimSpace(ee.Stderr))
		}
		if errors.As(runErr, &ee) {
			// sshd connected but the Gate ended without a result (killed,
			// or the Server went away mid-request).
			return fmt.Errorf("gate on %s stopped without a result (exit %d)%s\nhint: see `yoho releases` and `yoho app ps`; a deploy may have finished on the Server", g.Name(), ee.Code, stderrSuffix(ee.Stderr))
		}
		return runErr
	}
	return fmt.Errorf("gate on %s ended without a result", g.Name())
}

func stderrSuffix(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return ": " + s
	}
	return ""
}

// EncodeGateRequest renders the request line.
func EncodeGateRequest(app, destination, verb string, args any) ([]byte, error) {
	req := GateRequest{Protocol: GateProtocol, App: app, Destination: destination, Verb: verb}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		req.Args = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxGateRequest {
		return nil, fmt.Errorf("gate request is %d bytes, more than %d", len(b), MaxGateRequest)
	}
	return append(b, '\n'), nil
}

// ReadGateRequest reads the request line from r. The returned reader yields
// what follows it (the payload); r must not be read directly afterwards.
func ReadGateRequest(r io.Reader) (*GateRequest, *bufio.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxGateRequest {
			return nil, nil, fmt.Errorf("request larger than %d bytes", MaxGateRequest)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) > 0 {
			break
		}
		return nil, nil, fmt.Errorf("read request: %w", err)
	}
	var req GateRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, nil, fmt.Errorf("request is not valid JSON: %w", err)
	}
	if req.Protocol != GateProtocol {
		return nil, nil, fmt.Errorf("protocol %d is not supported (the Gate speaks %d); use the same yoho version as the Server", req.Protocol, GateProtocol)
	}
	return &req, br, nil
}

// FrameWriter writes GateFrames. Out frames may be written concurrently.
type FrameWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewFrameWriter writes frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (f *FrameWriter) frame(fr GateFrame) error {
	b, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, err := f.w.Write(append(b, '\n')); err != nil {
		f.err = err
	}
	return f.err
}

// Write sends p as an out frame (io.Writer for progress output).
func (f *FrameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := f.frame(GateFrame{Out: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Err is the first write error (the client went away).
func (f *FrameWriter) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Result sends the final result frame.
func (f *FrameWriter) Result(v any) error {
	if v == nil {
		v = struct{}{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.frame(GateFrame{Result: b})
}

// Fail sends the final error frame.
func (f *FrameWriter) Fail(e *GateError) error { return f.frame(GateFrame{Error: e}) }

// frameDecoder splits the Gate's stdout into frames.
type frameDecoder struct {
	out     io.Writer
	final   chan struct{} // closed at the result or error frame (nil: unused)
	closed  bool
	buf     []byte
	result  json.RawMessage
	gateErr *GateError
	bad     error
}

func (d *frameDecoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		i := bytes.IndexByte(d.buf, '\n')
		if i < 0 {
			if len(d.buf) > MaxGateRequest {
				d.bad = errors.New("gate sent an oversized frame")
				d.buf = nil
			}
			return len(p), nil
		}
		d.line(d.buf[:i])
		d.buf = d.buf[i+1:]
	}
}

func (d *frameDecoder) line(b []byte) {
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	var fr GateFrame
	if err := json.Unmarshal(b, &fr); err != nil {
		// Not a frame (e.g. a login banner): show it rather than drop it.
		fmt.Fprintf(d.out, "%s\n", b)
		return
	}
	if fr.Out != "" {
		io.WriteString(d.out, fr.Out)
	}
	if fr.Result != nil {
		d.result = fr.Result
	}
	if fr.Error != nil {
		d.gateErr = fr.Error
	}
	if (fr.Result != nil || fr.Error != nil) && d.final != nil && !d.closed {
		d.closed = true
		close(d.final)
	}
}

func (d *frameDecoder) finish() error {
	if len(d.buf) > 0 {
		d.line(d.buf)
		d.buf = nil
	}
	return d.bad
}

// maxChunk bounds one payload chunk.
const maxChunk = 1 << 20

// writeChunks frames r as chunks: a 4-byte big-endian length, then the
// bytes; a zero length ends the payload.
func writeChunks(w io.Writer, r io.Reader) error {
	buf := make([]byte, 4+maxChunk)
	for {
		n, err := io.ReadFull(r, buf[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(buf, uint32(n))
			if _, werr := w.Write(buf[:4+n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			binary.BigEndian.PutUint32(buf, 0)
			_, werr := w.Write(buf[:4])
			return werr
		}
		if err != nil {
			return err
		}
	}
}

// GatePayload reads the chunked payload that follows a request from r, up
// to its end marker.
func GatePayload(r io.Reader) io.Reader { return &chunkReader{r: r} }

type chunkReader struct {
	r    io.Reader
	left uint32
	done bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for c.left == 0 {
		if c.done {
			return 0, io.EOF
		}
		var hdr [4]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return 0, fmt.Errorf("payload: %w", io.ErrUnexpectedEOF)
		}
		c.left = binary.BigEndian.Uint32(hdr[:])
		if c.left == 0 {
			c.done = true
			return 0, io.EOF
		}
		if c.left > maxChunk {
			return 0, errors.New("payload: oversized chunk")
		}
	}
	if uint32(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= uint32(n)
	if err == io.EOF && c.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}
