package composefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

func TestCheckServerBuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "npm.txt"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Result{Project: &types.Project{WorkingDir: dir, Secrets: types.Secrets{"npm": {File: "npm.txt"}},
		Services: types.Services{"web": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: ".",
			Secrets: []types.ServiceSecretConfig{{Source: "npm"}}}}}}}}
	if f := CheckServerBuild(r, dir, config.Builder{Location: "local"}); f != nil {
		t.Fatalf("local builds are not checked: %v", f)
	}
	f := CheckServerBuild(r, dir, config.Builder{Location: "server"})
	if len(f) != 1 || f[0].Level != LevelWarning || f[0].Service != "web" || !strings.Contains(f[0].Message, "build secret npm") {
		t.Fatalf("%v", f)
	}
	r.Project.Services["web"].Build.Context = "../elsewhere"
	f = CheckServerBuild(r, dir, config.Builder{Location: "server"})
	if len(f) != 1 || f[0].Level != LevelError {
		t.Fatalf("%v", f)
	}
}
