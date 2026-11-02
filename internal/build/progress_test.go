package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

func rawLine(vertexes, logs string) string {
	return fmt.Sprintf(`{"vertexes":[%s],"logs":[%s]}`+"\n", vertexes, logs)
}

func vtx(d, name, started, completed string, cached bool, errMsg string) string {
	f := func(v string) string {
		if v == "" {
			return "null"
		}
		return `"` + v + `"`
	}
	return fmt.Sprintf(`{"digest":%q,"name":%q,"started":%s,"completed":%s,"cached":%t,"error":%q}`, d, name, f(started), f(completed), cached, errMsg)
}

const ts = "2026-01-01T00:00:00Z"

type seen struct {
	step                string
	done, steps, cached int
}

func TestProgressParser(t *testing.T) {
	var out bytes.Buffer
	var got []seen
	p := newProgressParser(&out, func(step string, done, steps, cached int) { got = append(got, seen{step, done, steps, cached}) })
	logData := base64.StdEncoding.EncodeToString([]byte("hello\nwor"))
	in := "ERROR: pre-progress noise\n" +
		rawLine(vtx("a", "[internal] load build definition", ts, "", false, ""), "") +
		rawLine(vtx("a", "", ts, ts, false, "")+","+vtx("b", "[1/2] FROM\ndocker.io/x", ts, ts, true, "")+","+vtx("c", "[2/2] RUN make", ts, "", false, ""), "") +
		rawLine("", `{"vertex":"c","stream":1,"data":"`+logData+`"}`) +
		rawLine("", `{"vertex":"c","stream":1,"data":"`+base64.StdEncoding.EncodeToString([]byte("ld\n"))+`"}`) +
		rawLine(vtx("c", "[2/2] RUN make", ts, ts, false, "exit code 2"), "")
	p.Write([]byte(in))
	p.Close()
	if len(got) != 3 {
		t.Fatalf("callbacks %v (logs must not trigger)", got)
	}
	if want := (seen{"[2/2] RUN make", 2, 3, 1}); got[1] != want {
		t.Fatalf("got %+v want %+v", got[2], want)
	}
	if last := got[2]; last.done != 3 || last.steps != 3 || last.cached != 1 {
		t.Fatalf("last %+v", last)
	}
	for _, w := range []string{"ERROR: pre-progress noise\n", "=> [1/2] FROM docker.io/x\n", "   | hello\n", "   | world\n", "ERROR: [2/2] RUN make: exit code 2\n"} {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("missing %q in\n%s", w, out.String())
		}
	}
	if strings.Contains(out.String(), "[internal]") {
		t.Fatalf("internal step shown:\n%s", out.String())
	}
	if p.rejectedRawJSON() {
		t.Fatal("not rejected once JSON seen")
	}
}

func TestProgressParserRejection(t *testing.T) {
	p := newProgressParser(io.Discard, nil)
	p.Write([]byte(`ERROR: invalid argument "rawjson" for "--progress" flag`))
	p.Close()
	if !p.rejectedRawJSON() {
		t.Fatal("want rejected")
	}
}

func progressOptions(r *recExec, prog func(BuildProgress)) Options {
	return Options{Dir: "/src", App: "shop", Version: "v1", Project: project("/src"),
		Builder: config.Builder{Engine: EngineDocker}, Exec: r.exec, Progress: prog}
}

func TestBuildProgressArgv(t *testing.T) {
	r := &recExec{}
	if _, err := Images(context.Background(), progressOptions(r, nil)); err != nil {
		t.Fatal(err)
	}
	c, _ := r.find("docker buildx build")
	if strings.Contains(strings.Join(c.Argv, " "), "--progress") {
		t.Fatalf("progress flag without Progress: %v", c.Argv)
	}
	r = &recExec{}
	if _, err := Images(context.Background(), progressOptions(r, func(BuildProgress) {})); err != nil {
		t.Fatal(err)
	}
	c, _ = r.find("docker buildx build")
	if !strings.Contains(strings.Join(c.Argv, " "), "--load --progress=rawjson ") {
		t.Fatalf("argv %v", c.Argv)
	}
}

func TestBuildProgressReported(t *testing.T) {
	r := &recExec{}
	o := progressOptions(r, nil)
	var got []BuildProgress
	o.Progress = func(p BuildProgress) { got = append(got, p) }
	o.Exec = func(ctx context.Context, c Command) error {
		if strings.HasPrefix(strings.Join(c.Argv, " "), "docker buildx build") {
			io.WriteString(c.Stderr, rawLine(vtx("a", "[1/1] RUN x", ts, "", false, ""), ""))
		}
		return r.exec(ctx, c)
	}
	if _, err := Images(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Service != "web" || got[0].Index != 1 || got[0].Total != 1 || got[0].Step != "[1/1] RUN x" {
		t.Fatalf("got %+v", got)
	}
}

func TestBuildProgressFallback(t *testing.T) {
	r := &recExec{}
	var builds int
	o := progressOptions(r, func(BuildProgress) {})
	o.Project.Services["web"].Build.DockerfileInline = "FROM scratch"
	o.Exec = func(ctx context.Context, c Command) error {
		a := strings.Join(c.Argv, " ")
		if strings.HasPrefix(a, "docker buildx build") {
			builds++
			if strings.Contains(a, "--progress=rawjson") {
				io.WriteString(c.Stderr, "ERROR: invalid progress mode rawjson\n")
				return fmt.Errorf("exit 1")
			}
			if b, _ := io.ReadAll(c.Stdin); string(b) != "FROM scratch" {
				t.Errorf("stdin not replayed: %q", b)
			}
		}
		return r.exec(ctx, c)
	}
	if _, err := Images(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("builds %d", builds)
	}
	// A real build failure is not retried.
	builds = 0
	o.Exec = func(ctx context.Context, c Command) error {
		if strings.HasPrefix(strings.Join(c.Argv, " "), "docker buildx build") {
			builds++
			io.WriteString(c.Stderr, rawLine(vtx("a", "RUN x", ts, ts, false, "boom"), ""))
			return fmt.Errorf("exit 1")
		}
		return nil
	}
	if _, err := Images(context.Background(), o); err == nil || builds != 1 {
		t.Fatalf("err %v builds %d", err, builds)
	}
}

func TestOnBuilt(t *testing.T) {
	r := &recExec{}
	o := progressOptions(r, nil)
	var names []string
	o.OnBuilt = func(s string, err error, _ time.Duration) { names = append(names, s) }
	if _, err := Images(context.Background(), o); err != nil || len(names) != 1 || names[0] != "web" {
		t.Fatalf("%v %v", err, names)
	}
}
