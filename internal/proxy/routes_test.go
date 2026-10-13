package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

func TestRouteConflictPinnedSemantics(t *testing.T) {
	for _, tc := range []struct {
		name              string
		desired, existing []string
		paths             []string
		same, conflict    bool
	}{
		{name: "rename", desired: []string{"a.example.com"}, existing: []string{"a.example.com"}, conflict: true},
		{name: "same-name update", desired: []string{"a.example.com"}, existing: []string{"a.example.com"}, same: true},
		{name: "case literal", desired: []string{"A.example.com"}, existing: []string{"a.example.com"}},
		{name: "port literal", desired: []string{"a.example.com:80"}, existing: []string{"a.example.com"}},
		{name: "trailing dot literal", desired: []string{"a.example.com."}, existing: []string{"a.example.com"}},
		{name: "whitespace literal", desired: []string{" a.example.com"}, existing: []string{"a.example.com"}},
		{name: "CSV aliases", desired: []string{"a.example.com,b.example.com"}, existing: []string{"b.example.com"}, conflict: true},
		{name: "quoted CSV", desired: []string{`"a.example.com,b.example.com"`}, existing: []string{"a.example.com,b.example.com"}, conflict: true},
		{name: "live hosts already decoded", desired: []string{"a.example.com"}, existing: []string{"a.example.com,b.example.com"}},
		{name: "wildcard same", desired: []string{"*.example.com"}, existing: []string{"*.example.com"}, conflict: true},
		{name: "wildcard specific", desired: []string{"a.example.com"}, existing: []string{"*.example.com"}},
		{name: "catchall specific", desired: []string{"a.example.com"}},
		{name: "catchall same", conflict: true},
		{name: "empty flag catchall", desired: []string{""}, existing: []string{""}, conflict: true},
		{name: "quoted empty catchall", desired: []string{`""`}, existing: []string{""}, conflict: true},
		{name: "path route coexists", desired: []string{"a.example.com"}, existing: []string{"a.example.com"}, paths: []string{"/api"}},
		{name: "normalized root prefix", desired: []string{"a.example.com"}, existing: []string{"a.example.com"}, paths: []string{"///"}, conflict: true},
		{name: "root among prefixes", desired: []string{"a.example.com"}, existing: []string{"a.example.com"}, paths: []string{"/api", "/"}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired, err := DesiredRoute("new", tc.desired)
			if err != nil {
				t.Fatal(err)
			}
			name := "old"
			if tc.same {
				name = "new"
			}
			live := Route{Service: name, Hosts: tc.existing, PathPrefixes: tc.paths, State: "stopped"}
			err = CheckRouteConflicts([]Route{desired}, []Route{live})
			var conflict *RouteConflictError
			if errors.As(err, &conflict) != tc.conflict {
				t.Fatalf("conflict=%v err=%v", tc.conflict, err)
			}
			if conflict != nil && (conflict.Desired != "new" || conflict.Existing != "old") {
				t.Fatalf("wrong routes: %+v", conflict)
			}
			// Local desired duplicates obey the very same engine rules.
			err = CheckRouteConflicts([]Route{live, desired}, nil)
			if (err != nil) != tc.conflict {
				t.Fatalf("local conflict=%v err=%v", tc.conflict, err)
			}
		})
	}
	if _, err := DesiredRoute("web", []string{`"unterminated`}); err == nil {
		t.Fatal("malformed CSV accepted")
	}
	rt, err := DesiredRoute("web", []string{"", "a.example.com", "a.example.com"})
	if err != nil || !reflect.DeepEqual(rt.Hosts, []string{"a.example.com", "a.example.com"}) {
		t.Fatalf("empty flag/duplicate same-route host: %+v %v", rt, err)
	}
	if err := CheckRouteConflicts([]Route{rt}, nil); err != nil {
		t.Fatal(err)
	}
	live, err := ParseRoutes(`{"api":{"hosts":["a.example.com"],"path_prefixes":["/api"],"targets":["api:80"]}}`)
	if err != nil || len(live) != 1 || !reflect.DeepEqual(live[0].PathPrefixes, []string{"/api"}) {
		t.Fatalf("path metadata lost: %+v %v", live, err)
	}
}

func TestRouteDisplayedCatchallAmbiguity(t *testing.T) {
	for _, hosts := range [][]string{nil, {"*"}, {"app.example.com"}, {"*.example.com"}} {
		want, err := DesiredRoute("new", hosts)
		if err != nil {
			t.Fatal(err)
		}
		live, err := ParseRoutes(`{"old":{"hosts":["*"],"path_prefixes":["/"],"targets":["old:80"]}}`)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckRouteConflicts([]Route{want}, live)
		var conflict *RouteConflictError
		blocked := len(hosts) == 0 || hosts[0] == "*"
		if errors.As(err, &conflict) != blocked {
			t.Fatalf("hosts=%v err=%v", hosts, err)
		}
		if conflict != nil && !conflict.DisplayAmbiguous {
			t.Fatal("display ambiguity not explained")
		}
		if live[0].Hosts[0] != "*" {
			t.Fatal("public List display changed")
		}
		live[0].PathPrefixes = []string{"/api"}
		if err := CheckRouteConflicts([]Route{want}, live); err != nil {
			t.Fatalf("distinct path rejected: %v", err)
		}
	}
	// Unlike live List, known desired literal '*' and catch-all are distinct.
	catchall, _ := DesiredRoute("catchall", nil)
	literal, _ := DesiredRoute("literal", []string{"*"})
	if err := CheckRouteConflicts([]Route{catchall, literal}, nil); err != nil {
		t.Fatalf("known local hosts over-normalized: %v", err)
	}
}

func TestListFailsClosedLocalShell(t *testing.T) {
	// Every docker command is a disposable local shim: no real Docker/Server.
	dir := t.TempDir()
	shim := "#!/bin/sh\nset -eu\ncase \"$1 $2\" in\n'container ls')\n  case \"$PROXY_TEST_MODE\" in missing) exit 0;; daemon) echo 'daemon unavailable' >&2; exit 42;; *) printf 'yoho-proxy\\n';; esac;;\n'exec yoho-proxy')\n  case \"$PROXY_TEST_MODE\" in query) echo 'proxy stopped' >&2; exit 43;; empty) exit 0;; malformed) printf 'invalid\\n';; *) printf '{}\\n';; esac;;\n*) echo 'unexpected docker command' >&2; exit 99;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []string{"missing", "running", "daemon", "query", "empty", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PROXY_TEST_MODE", mode)
			routes, err := List(context.Background(), &remote.Local{})
			wantErr := mode == "daemon" || mode == "query" || mode == "empty" || mode == "malformed"
			if (err != nil) != wantErr || len(routes) != 0 {
				t.Fatalf("routes=%v err=%v", routes, err)
			}
			if err != nil && strings.Contains(err.Error(), "unexpected docker") {
				t.Fatal(err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &fakeHost{}
	if _, err := List(ctx, h); !errors.Is(err, context.Canceled) || len(h.scripts) != 0 {
		t.Fatalf("canceled query: %v %v", err, h.scripts)
	}
}
