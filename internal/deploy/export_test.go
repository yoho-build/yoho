package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestExports(t *testing.T) {
	ctx := context.Background()
	if !ValidVersion("abc.1") || ValidVersion("../x") {
		t.Error("ValidVersion")
	}
	out, err := EscapeDollars([]byte("a: x$y\n"))
	if err != nil || !strings.Contains(string(out), "x$$y") {
		t.Errorf("EscapeDollars: %q %v", out, err)
	}
	withRoot(t)
	h := &remote.Local{}
	p := release.Dir("a", "b", "v1") + "/release.json"
	if err := WriteJSON(ctx, h, p, release.Release{App: "a", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	r, err := ReadRelease(ctx, h, p)
	if err != nil || r.Version != "v1" {
		t.Fatalf("ReadRelease: %+v %v", r, err)
	}
	rels, err := ListReleases(ctx, h, "a", "b")
	if err != nil || len(rels) != 1 {
		t.Fatalf("ListReleases: %v %v", rels, err)
	}
	if len(SHA256Hex(nil)) != 64 || string(EnvFile(map[string]string{"A": "1"})) != "A=\"1\"\n" {
		t.Error("SHA256Hex/EnvFile")
	}
}
