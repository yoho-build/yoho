package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxPayload is the largest POST /jobs body, in bytes.
const maxPayload = 256

// marker is compiled into the binary so a one-line edit changes the image.
const marker = "hello-v1"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err := withDB(context.Background(), migrate); err != nil {
				log.Fatal(err)
			}
			return
		case "worker":
			if err := runWorker(); err != nil {
				log.Fatal(err)
			}
			return
		case "health":
			if err := withDB(context.Background(), func(ctx context.Context, pool *pgxpool.Pool) error {
				return pool.Ping(ctx)
			}); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		host, _ := os.Hostname()
		writeJSON(w, map[string]string{
			"version":  os.Getenv("YOHO_VERSION"),
			"marker":   marker,
			"hostname": host,
		})
	})
	mux.HandleFunc("/greeting", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		g, err := secret("APP_GREETING")
		if err != nil {
			http.Error(w, "greeting unavailable", http.StatusServiceUnavailable)
			return
		}
		length, prefix := fingerprint(g)
		writeJSON(w, map[string]any{"length": length, "sha256_8": prefix})
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rows, err := pool.Query(r.Context(), `SELECT id, payload, status, greeting_len, created_at, done_at FROM jobs ORDER BY id DESC LIMIT 50`)
			if err != nil {
				http.Error(w, "query", http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			type job struct {
				ID          int64      `json:"id"`
				Payload     string     `json:"payload"`
				Status      string     `json:"status"`
				GreetingLen *int       `json:"greeting_len"`
				CreatedAt   time.Time  `json:"created_at"`
				DoneAt      *time.Time `json:"done_at"`
			}
			out := []job{}
			for rows.Next() {
				var j job
				if err := rows.Scan(&j.ID, &j.Payload, &j.Status, &j.GreetingLen, &j.CreatedAt, &j.DoneAt); err != nil {
					http.Error(w, "scan", http.StatusInternalServerError)
					return
				}
				out = append(out, j)
			}
			if err := rows.Err(); err != nil {
				http.Error(w, "rows", http.StatusInternalServerError)
				return
			}
			writeJSON(w, out)
		case http.MethodPost:
			// The demo is public behind a Quick Tunnel: keep payloads small.
			body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
			if err != nil {
				http.Error(w, "body", http.StatusBadRequest)
				return
			}
			if len(body) > maxPayload {
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			payload := strings.TrimSpace(string(body))
			if payload == "" {
				payload = "hello"
			}
			if strings.ContainsAny(payload, "\r\n") {
				http.Error(w, "payload must be one line", http.StatusBadRequest)
				return
			}
			var id int64
			if err := pool.QueryRow(r.Context(), `INSERT INTO jobs (payload) VALUES ($1) RETURNING id`, payload).Scan(&id); err != nil {
				http.Error(w, "insert", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": id, "payload": payload})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		g, err := secret("APP_GREETING")
		length, prefix := 0, "unavailable"
		if err == nil {
			length, prefix = fingerprint(g)
		}
		host, _ := os.Hostname()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<meta charset="utf-8">
<title>Yoho shop</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem;color:#17342d;background:#f7faf9}
h1{letter-spacing:-.03em}
code,pre{background:#eef4f1;padding:.1rem .35rem}
.card{border-inline-start:4px solid #14bba6;background:#eef4f1;padding:1rem 1.2rem;margin:1rem 0}
</style>
<h1>Yoho shop</h1>
<p>web + worker + Postgres on your Server.</p>
<div class="card">
<p>marker <code>%s</code></p>
<p>version <code>%s</code> · host <code>%s</code></p>
<p>greeting fingerprint <code>%s</code> (%d chars) — the value is never shown</p>
</div>
<p>POST a one-line payload to <code>/jobs</code>. The worker records the greeting length (not the value) on each job.</p>
`, html.EscapeString(marker), html.EscapeString(os.Getenv("YOHO_VERSION")), html.EscapeString(host), html.EscapeString(prefix), length)
	})

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdown <- srv.Shutdown(shut)
	}()
	log.Printf("listen :8080 %s", marker)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown starts; let in-flight
	// requests finish before the pool closes.
	return <-shutdown
}

func runWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Printf("worker %s", marker)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := processJobs(ctx, pool); err != nil && ctx.Err() == nil {
			log.Printf("process: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func processJobs(ctx context.Context, pool *pgxpool.Pool) error {
	g, err := secret("APP_GREETING")
	if err != nil {
		return err
	}
	length, _ := fingerprint(g)
	for {
		var id int64
		err := pool.QueryRow(ctx, `
			UPDATE jobs SET status = 'done', greeting_len = $1, done_at = now()
			WHERE id = (SELECT id FROM jobs WHERE status = 'pending' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
			RETURNING id
		`, length).Scan(&id)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		log.Printf("processed job %d greeting_len=%d", id, length)
	}
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS jobs (
			id BIGSERIAL PRIMARY KEY,
			payload TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			greeting_len INT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			done_at TIMESTAMPTZ
		)
	`)
	return err
}

func withDB(ctx context.Context, fn func(context.Context, *pgxpool.Pool) error) error {
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(ctx, pool)
}

func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	password, err := secret("POSTGRES_PASSWORD")
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig("postgres://postgres@db:5432/postgres?sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Password = password
	var last error
	for i := 0; i < 30; i++ {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("postgres: %w", last)
}

// secret reads NAME from NAME_FILE (Yoho file delivery) or NAME (local compose).
func secret(name string) (string, error) {
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s or %s_FILE is required", name, name)
}

func fingerprint(v string) (int, string) {
	sum := sha256.Sum256([]byte(v))
	return len(v), hex.EncodeToString(sum[:4])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
