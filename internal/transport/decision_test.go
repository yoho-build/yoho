package transport

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestFallback(t *testing.T) {
	for _, reason := range []string{"plugin", "target", "probe", "store", "pussh", "load", "container", "gate"} {
		t.Run(reason, func(t *testing.T) {
			d := Fallback(reason)
			mode := MethodLoad
			if reason == "load" {
				mode = MethodRegistry
			}
			if d.Mode != mode || d.Reason == "" || d.Hint == "" {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	for _, tc := range []struct {
		name, engine, target string
		plugin, store        bool
		reason               string
	}{
		{name: "plugin", target: "u@s", reason: "plugin"},
		{name: "target", plugin: true, reason: "target"},
		{name: "store", plugin: true, target: "u@s", reason: "store"},
		{name: "container", engine: EngineContainer, reason: "container"},
		{name: "pussh", plugin: true, store: true, target: "u@s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHost{name: "s", containerd: tc.store}
			f := &fakeLocal{pussh: tc.plugin}
			choices, err := Preview(context.Background(), PushOptions{Hosts: []remote.Host{h}, Engine: tc.engine, Exec: f.exec, Targets: map[string]string{"s": tc.target}})
			if err != nil {
				t.Fatal(err)
			}
			got := choices["s"]
			if tc.reason != "" {
				if got != Fallback(tc.reason) {
					t.Fatalf("%+v", got)
				}
			} else if got.Mode != MethodPussh {
				t.Fatalf("%+v", got)
			}
			if f.has("docker save") || f.has("docker pussh x") {
				t.Fatal("preview shipped an image")
			}
		})
	}
}

func TestMissingBlobs(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		required, present, want []string
	}{
		{name: "empty"},
		{name: "cold", required: []string{"sha256:a", "sha256:b"}, want: []string{"sha256:a", "sha256:b"}},
		{name: "warm", required: []string{"sha256:a", "sha256:b"}, present: []string{"sha256:a"}, want: []string{"sha256:b"}},
		{name: "cached", required: []string{"sha256:a"}, present: []string{"sha256:a"}},
		{name: "duplicate", required: []string{"sha256:a", "sha256:a"}, want: []string{"sha256:a"}},
		{name: "unrelated", required: []string{"sha256:a"}, present: []string{"sha256:diffid"}, want: []string{"sha256:a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MissingBlobs(tc.required, tc.present); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%v want %v", got, tc.want)
			}
		})
	}
}

func TestFallbackCallback(t *testing.T) {
	h := &fakeHost{name: "s", loadFails: true}
	f := &fakeLocal{}
	var got []Decision
	_, _ = Push(context.Background(), PushOptions{Images: []string{"x:1"}, Hosts: []remote.Host{h}, Exec: f.exec, OnDecision: func(host string, d Decision) {
		if host != "s" {
			t.Error(host)
		}
		got = append(got, d)
	}})
	if len(got) != 1 || got[0] != Fallback("plugin") {
		t.Fatalf("%+v", got)
	}
}

type failingProbeHost struct{ *fakeHost }

func (h failingProbeHost) Output(context.Context, remote.Cmd) (string, error) {
	return "", fmt.Errorf("private diagnostic")
}

func TestPusshProbeFailureDecision(t *testing.T) {
	h := failingProbeHost{&fakeHost{name: "s"}}
	f := &fakeLocal{pussh: true}
	choices, err := Preview(context.Background(), PushOptions{Hosts: []remote.Host{h}, Targets: map[string]string{"s": "u@s"}, Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	got := choices["s"]
	if got != Fallback("probe") {
		t.Fatalf("%+v", got)
	}
}

func TestRuntimeFallbackDecisions(t *testing.T) {
	for _, fail := range []string{"pussh", "load"} {
		t.Run(fail, func(t *testing.T) {
			h := &fakeHost{name: "s", containerd: true, loadFails: fail == "load"}
			f := &fakeLocal{pussh: fail == "pussh"}
			var got []Decision
			localExec := func(ctx context.Context, c Command) error {
				if fail == "pussh" && len(c.Argv) > 2 && c.Argv[1] == "pussh" && c.Argv[2] != "--help" {
					return fmt.Errorf("private diagnostic")
				}
				return f.exec(ctx, c)
			}
			_, err := Push(context.Background(), PushOptions{Images: []string{"registry.example/app:1"}, Hosts: []remote.Host{h}, Targets: map[string]string{"s": "u@s"}, Registry: &config.Registry{Prefix: "registry.example"}, Exec: localExec, OnDecision: func(_ string, d Decision) { got = append(got, d) }})
			if err != nil {
				t.Fatal(err)
			}
			if fail == "load" {
				if len(got) != 2 || got[0] != Fallback("plugin") || got[1] != Fallback("load") {
					t.Fatalf("%+v", got)
				}
			} else if len(got) != 1 || got[0] != Fallback("pussh") {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestPreviewExplicitContainerModes(t *testing.T) {
	for _, mode := range []string{"", "auto", MethodLoad, MethodPussh, MethodRegistry, "invalid"} {
		t.Run(mode, func(t *testing.T) {
			h := &fakeHost{name: "s"}
			o := PushOptions{Hosts: []remote.Host{h}, Images: []string{"x:1"}, Mode: mode, Engine: EngineContainer,
				Exec: func(context.Context, Command) error { t.Fatal("executed during mode validation"); return nil }}
			choices, err := Preview(context.Background(), o)
			invalid := mode == MethodPussh || mode == MethodRegistry || mode == "invalid"
			if invalid {
				_, pushErr := Push(context.Background(), o)
				if err == nil || pushErr == nil || err.Error() != pushErr.Error() {
					t.Fatalf("preview=%v push=%v", err, pushErr)
				}
			} else if err != nil || choices["s"] != Fallback("container") {
				t.Fatalf("%+v %v", choices, err)
			}
		})
	}
}

func TestReportWithoutHint(t *testing.T) {
	var out bytes.Buffer
	p := &pusher{out: &out}
	p.report(&fakeHost{name: "s"}, Decision{Mode: MethodLoad, Reason: "explicit transport"})
	if out.String() != "s: load: explicit transport\n" {
		t.Fatal(out.String())
	}
}
