package config

import (
	"strings"
	"testing"
)

func TestBuilderDirect(t *testing.T) {
	base := "app: a\nservers: {w: {ssh: x}}\ndestinations: {p: {servers: [w]}}\n"
	for _, tc := range []struct {
		src  string
		want BuilderDirect
		bad  string
	}{
		{"builder: {location: remote, remote: b}\n", DirectAuto, ""},
		{"builder: {location: remote, remote: b, direct: true}\n", DirectOn, ""},
		{"builder: {location: remote, remote: b, direct: false}\n", DirectOff, ""},
		{"builder: {location: remote, remote: b, direct: auto}\n", DirectAuto, ""},
		{"builder: {location: local, direct: true}\n", DirectOn, "needs builder.location remote"},
		{"builder: {location: remote, remote: b, direct: true, platforms: [linux/amd64, linux/arm64]}\n", DirectOn, "ships one platform"},
		{"builder: {location: remote, remote: b, direct: true}\ntransport: {mode: pussh}\n", DirectOn, "transport.mode pussh"},
	} {
		c, err := Parse([]byte(base+tc.src), "t")
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got := c.Builder.Direct.Mode(); got != tc.want {
			t.Errorf("%s: mode %q, want %q", tc.src, got, tc.want)
		}
		var msgs []string
		for _, e := range Validate(c) {
			msgs = append(msgs, e.Error())
		}
		joined := strings.Join(msgs, "\n")
		if tc.bad == "" && strings.Contains(joined, "builder.direct") || tc.bad != "" && !strings.Contains(joined, tc.bad) {
			t.Errorf("%s: validate %q, want %q", tc.src, joined, tc.bad)
		}
	}
	if _, err := Parse([]byte(base+"builder: {direct: sometimes}\n"), "t"); err == nil || !strings.Contains(err.Error(), "auto, true or false") {
		t.Errorf("bad value: %v", err)
	}
	if _, err := Parse([]byte(`{"app":"a","builder":{"direct":"true"}}`), "t.json"); err != nil {
		t.Errorf("string form: %v", err)
	}
	b, err := Schema()
	if err != nil || !strings.Contains(string(b), `"direct"`) {
		t.Errorf("schema lacks builder.direct: %v", err)
	}
}
