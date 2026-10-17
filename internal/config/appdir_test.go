package config

import (
	"strings"
	"testing"
)

func appDirErrors(t *testing.T, src string) string {
	t.Helper()
	c, err := Parse([]byte(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range Validate(c) {
		all = append(all, e.Error())
	}
	return strings.Join(all, "\n")
}

func TestValidateAppDir(t *testing.T) {
	const servers = "app: elpoca\nservers: {small-deb: {ssh: deb@small, root: /home/deb/apps/yoho}, other: {ssh: deb@other, root: /home/deb/apps/yoho}, alias: {ssh: deb@small, root: /home/deb/apps/yoho}, def: {ssh: x}}\n"
	cases := []struct {
		name, dests, want string
	}{
		{"valid sibling of root", "destinations: {staging: {servers: [small-deb], app_dir: /home/deb/apps/elpoca-engine}}", ""},
		{"valid outside default root", "destinations: {staging: {servers: [def], app_dir: /srv/elpoca}}", ""},
		{"relative", "destinations: {staging: {servers: [small-deb], app_dir: apps/elpoca}}", "must be an absolute path"},
		{"slash", "destinations: {staging: {servers: [small-deb], app_dir: /}}", "must not be /"},
		{"trailing slash", "destinations: {staging: {servers: [small-deb], app_dir: /srv/elpoca/}}", "must be a clean path"},
		{"dot dot", "destinations: {staging: {servers: [small-deb], app_dir: /srv/x/../elpoca}}", "must be a clean path"},
		{"double slash", "destinations: {staging: {servers: [small-deb], app_dir: //srv/elpoca}}", "must be a clean path"},
		{"unsafe characters", `destinations: {staging: {servers: [small-deb], app_dir: "/srv/my app"}}`, "may only use letters"},
		{"equal to root", "destinations: {staging: {servers: [small-deb], app_dir: /home/deb/apps/yoho}}", "must be outside the Server root"},
		{"inside root", "destinations: {staging: {servers: [small-deb], app_dir: /home/deb/apps/yoho/apps/elpoca}}", "must be outside the Server root"},
		{"inside default root", "destinations: {staging: {servers: [def], app_dir: /var/lib/yoho/elpoca}}", "must be outside the Server root"},
		{"ancestor of root", "destinations: {staging: {servers: [small-deb], app_dir: /home/deb}}", "must not contain the Server root"},
		{"prefix but not ancestor", "destinations: {staging: {servers: [small-deb], app_dir: /home/deb/apps/yoho-engine}}", ""},
		{"destination name pattern", "destinations: {Staging_1: {servers: [small-deb], app_dir: /srv/elpoca}}", "must match"},
		{"destination name fine without app_dir", "destinations: {Staging_1: {servers: [small-deb]}}", ""},
		{"duplicate on shared Server", "destinations: {a: {servers: [small-deb], app_dir: /srv/elpoca}, b: {servers: [small-deb], app_dir: /srv/elpoca}}", "same directory on a shared Server"},
		{"nested on shared Server", "destinations: {a: {servers: [small-deb], app_dir: /srv/elpoca}, b: {servers: [small-deb], app_dir: /srv/elpoca/b}}", "nested on a shared Server"},
		{"duplicate through the same SSH target", "destinations: {a: {servers: [small-deb], app_dir: /srv/elpoca}, b: {servers: [alias], app_dir: /srv/elpoca}}", "same directory on a shared Server"},
		{"same path on different Servers", "destinations: {a: {servers: [small-deb], app_dir: /srv/elpoca}, b: {servers: [other], app_dir: /srv/elpoca}}", ""},
		{"siblings on shared Server", "destinations: {a: {servers: [small-deb], app_dir: /srv/elpoca-a}, b: {servers: [small-deb], app_dir: /srv/elpoca}}", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appDirErrors(t, servers+c.dests+"\n")
			if c.want == "" && got != "" {
				t.Fatalf("unexpected errors:\n%s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want %q, got:\n%s", c.want, got)
			}
		})
	}
}

func TestValidateAppDirUnsafeRoot(t *testing.T) {
	got := appDirErrors(t, `app: elpoca
servers: {s: {ssh: x, root: "/home/deb/my apps"}}
destinations: {staging: {servers: [s], app_dir: /srv/elpoca}}
`)
	if !strings.Contains(got, "the Server root") || !strings.Contains(got, "when app_dir is set") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestSchemaHasAppDir(t *testing.T) {
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"app_dir"`) {
		t.Fatal("schema lacks app_dir")
	}
}
