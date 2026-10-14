// Package webui serves `yoho ui`: the embedded React dashboard and a small
// read-only JSON API over a dash.Live, on a loopback address only.
//
// Access: the CLI prints (and opens) a one-time login link. Visiting it
// trades the code for an HttpOnly, SameSite=Strict session cookie, so the
// session token never appears in argv, history, or the page. Every request
// must carry a loopback Host header (DNS-rebinding defence), and only GET and
// HEAD exist: the UI cannot change anything on a Server.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

//go:embed all:dist
var dist embed.FS

// Assets is the built web app (internal/webui/dist, from web/).
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// Data is what the API reads. *dash.Live implements it.
type Data interface {
	Snapshot(ctx context.Context, fresh bool) (*dash.Snapshot, error)
	Logs(ctx context.Context, server, name string, lines int) (string, error)
}

// Meta is static information for the page header.
type Meta struct {
	Version  string `json:"version"`
	Interval int    `json:"interval_seconds"`
}

const cookieName = "yoho_ui"

// Server is the HTTP handler of one `yoho ui` session.
type Server struct {
	data   Data
	meta   Meta
	assets fs.FS

	mu      sync.Mutex
	code    string // one-time login code, "" once used
	session string // session cookie value
	port    string // listener port, for the Host check
}

// New returns a Server with a fresh login code and session token.
func New(data Data, meta Meta, assets fs.FS) *Server {
	return &Server{data: data, meta: meta, assets: assets, code: token(), session: token()}
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// LoginURL is the one-time link for the listener at addr.
func (s *Server) LoginURL(addr net.Addr) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "http://" + addr.String() + "/login?code=" + s.code
}

// Serve listens until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Listen opens a loopback listener. Port 0 picks a free port.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if !s.loopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "read-only", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == "/login":
		s.login(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		h.Set("Cache-Control", "no-store")
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in", "hint": "open the login link printed by `yoho ui`"})
			return
		}
		s.api(w, r)
	default:
		s.static(w, r)
	}
}

// loopbackHost accepts only 127.0.0.1, localhost and [::1] on our port.
func (s *Server) loopbackHost(host string) bool {
	hn, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	s.mu.Lock()
	want := s.port
	s.mu.Unlock()
	if want != "" && port != want {
		return false
	}
	switch strings.ToLower(hn) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	s.mu.Lock()
	ok := s.code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.code)) == 1
	if ok {
		s.code = "" // single use
	}
	session := s.session
	s.mu.Unlock()
	if !ok {
		if s.authorized(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.Error(w, "this login link is invalid or was already used; restart `yoho ui` for a new one", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authorized(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) == 1
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.URL.Path {
	case "/api/meta":
		writeJSON(w, http.StatusOK, s.meta)
	case "/api/snapshot":
		snap, err := s.data.Snapshot(ctx, r.URL.Query().Get("fresh") == "1")
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, snap)
	case "/api/logs":
		q := r.URL.Query()
		lines, _ := strconv.Atoi(q.Get("lines"))
		out, err := s.data.Logs(ctx, q.Get("server"), q.Get("container"), lines)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"logs": out})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// static serves the embedded app; unknown paths get index.html.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if st, err := fs.Stat(s.assets, p); err != nil || st.IsDir() {
		p = "index.html"
	}
	if p == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFileFS(w, r, s.assets, p)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
