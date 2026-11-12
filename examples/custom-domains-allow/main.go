// Command custom-domains-allow is a reference on-demand TLS approval endpoint.
// Verify customer DNS ownership before adding a hostname to VERIFIED_FILE.
// See README.md for deployment and staging-only scale testing.
package main

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Verified is the App's source of verified customer hostnames.
// A database implementation could execute:
// SELECT 1 FROM custom_domains WHERE hostname=$1 AND verified_at IS NOT NULL
// using QueryRowContext(ctx, query, host); sql.ErrNoRows means false.
type Verified interface {
	IsVerified(ctx context.Context, host string) (bool, error)
}

func normalize(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return "", errors.New("invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid character")
			}
		}
	}
	return host, nil
}

type fileVerified struct {
	mu    sync.Mutex
	path  string
	mod   time.Time
	size  int64
	hosts map[string]bool
}

func (f *fileVerified) reload(ctx context.Context, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	if !force && f.hosts != nil && info.ModTime().Equal(f.mod) && info.Size() == f.size {
		return nil
	}
	file, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer file.Close()
	hosts := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, err := normalize(line)
		if err != nil {
			return err
		}
		hosts[host] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	f.hosts, f.mod, f.size = hosts, info.ModTime(), info.Size()
	return nil
}

func (f *fileVerified) IsVerified(ctx context.Context, host string) (bool, error) {
	if err := f.reload(ctx, false); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[host], nil
}

type suffixVerified string

func (s suffixVerified) IsVerified(ctx context.Context, host string) (bool, error) {
	return strings.HasSuffix(host, string(s)), ctx.Err()
}

type entry struct {
	allowed bool
	expires time.Time
}
type handler struct {
	source Verified
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]entry
}

func (h *handler) check(ctx context.Context, host string) (bool, error) {
	h.mu.Lock()
	e, ok := h.cache[host]
	h.mu.Unlock()
	if ok && h.now().Before(e.expires) {
		return e.allowed, nil
	}
	allowed, err := h.source.IsVerified(ctx, host)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return false, err
	}
	ttl := 30 * time.Second
	if allowed {
		ttl = 5 * time.Minute
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cache == nil {
		h.cache = make(map[string]entry)
	}
	if len(h.cache) >= 10000 {
		for k, v := range h.cache {
			if !h.now().Before(v.expires) {
				delete(h.cache, k)
			}
		}
	}
	if len(h.cache) < 10000 {
		h.cache[host] = entry{allowed, h.now().Add(ttl)}
	}
	return allowed, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	host, err := normalize(r.URL.Query().Get("host"))
	if err != nil {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	// Keep the HTTP response within the Proxy's 2s budget even if a source
	// fails to honor cancellation. Source implementations must honor ctx
	// to avoid leaking work after a timed-out request.
	type result struct {
		allowed bool
		err     error
	}
	done := make(chan result, 1)
	go func() { a, e := h.check(ctx, host); done <- result{a, e} }()
	select {
	case <-ctx.Done():
		http.Error(w, "verification unavailable", http.StatusServiceUnavailable)
	case res := <-done:
		if res.err != nil {
			http.Error(w, "verification unavailable", http.StatusServiceUnavailable)
		} else if !res.allowed {
			http.Error(w, "not verified", http.StatusForbidden)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}
}

func main() {
	var source Verified
	if suffix := os.Getenv("ALLOW_SUFFIX"); suffix != "" {
		host, err := normalize(strings.TrimPrefix(suffix, "."))
		if err != nil || !strings.HasPrefix(suffix, ".") {
			log.Fatal("ALLOW_SUFFIX must be a dot-prefixed DNS suffix")
		}
		log.Print("WARNING: ALLOW_SUFFIX test-only mode bypasses customer ownership verification; use only with tls_staging: true")
		source = suffixVerified("." + host)
	} else {
		path := os.Getenv("VERIFIED_FILE")
		if path == "" {
			path = "verified-hosts.txt"
		}
		f := &fileVerified{path: path}
		if err := f.reload(context.Background(), true); err != nil {
			log.Fatal(err)
		}
		source = f
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGHUP)
		go func() {
			for range signals {
				if err := f.reload(context.Background(), true); err != nil {
					log.Printf("reload failed: %v", err)
				}
			}
		}()
	}
	mux := http.NewServeMux()
	mux.Handle("/custom-domains/allow", &handler{source: source, now: time.Now})
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("scale App\n")) })
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Fatal((&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 30 * time.Second}).ListenAndServe())
}
