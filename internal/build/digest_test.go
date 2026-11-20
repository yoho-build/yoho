package build

import (
	"github.com/compose-spec/compose-go/v2/types"
	"os"
	"path/filepath"
	"testing"
)

func TestInputsDigestTracksSourcesAndExcludesSecrets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM busybox\nCOPY source /source\n")
	write("source", "one")
	write("ignored", "one")
	write(".dockerignore", "ignored\nsecret\n")
	write("secret", "private-value-one")
	p := &types.Project{WorkingDir: dir, Services: types.Services{"web": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: dir, Secrets: []types.ServiceSecretConfig{{Source: "key"}}}}}}, Secrets: types.Secrets{"key": {File: filepath.Join(dir, "secret")}}}
	o := Options{Dir: dir, Project: p}
	get := func() string {
		t.Helper()
		s, err := InputsDigest(o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := get()
	write("ignored", "two")
	write("secret", "private-value-two")
	if get() != first {
		t.Fatal("ignored content or secret affected digest")
	}
	write("source", "two")
	if get() == first {
		t.Fatal("source change was missed")
	}
}
