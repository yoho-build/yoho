package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
)

// YOHO_E2E=1 YOHO_E2E_SSH=user@host with local Docker: create 3 previews plus
// a non-preview production resource, run `yoho preview prune --keep 1 --yes`,
// and verify only the newest preview remains and no non-preview resource was touched.
func TestE2EPreviewPrune(t *testing.T) {
	target := os.Getenv("YOHO_E2E_SSH")
	if os.Getenv("YOHO_E2E") != "1" || target == "" {
		t.Skip("YOHO_E2E=1 and YOHO_E2E_SSH not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}

	ctx := context.Background()
	appName := fmt.Sprintf("e2eprune%d", time.Now().UnixNano()%1_000_000)
	rootDir := t.TempDir()
	workDir := t.TempDir()

	yohoCfg := fmt.Sprintf(`app: %s
root: %s
servers:
  s: {ssh: %s}
destinations:
  production: {servers: [s]}
  staging: {servers: [s]}
previews:
  from: staging
  host: "{name}.preview.local"
`, appName, rootDir, target)
	composeCfg := `services:
  web:
    image: busybox:latest
    command: ["sleep", "300"]
    x-yoho:
      proxy: {hosts: [staging.local], port: 80}
`
	if err := os.WriteFile(filepath.Join(workDir, "yoho.yml"), []byte(yohoCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "compose.yaml"), []byte(composeCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRoot := release.Root
	release.Root = rootDir
	t.Cleanup(func() { release.Root = oldRoot })

	dockerRun := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	prodProject := release.ProjectName(appName, "production")
	prodDir := release.AppDir(appName, "production")
	if err := os.MkdirAll(prodDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prodMarker := filepath.Join(prodDir, "keep.txt")
	if err := os.WriteFile(prodMarker, []byte("production"), 0o600); err != nil {
		t.Fatal(err)
	}
	prodVol := prodProject + "_data"
	prodContainer := prodProject + "-web-1"
	dockerRun("volume", "create", "--label", "com.docker.compose.project="+prodProject, prodVol)
	dockerRun("run", "-d", "--name", prodContainer,
		"--label", "com.docker.compose.project="+prodProject,
		"--label", "yoho.app="+appName,
		"--label", "yoho.destination=production",
		"-v", prodVol+":/data",
		"busybox:latest", "sleep", "300")
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-fv", prodContainer).Run()
		exec.Command("docker", "volume", "rm", "-f", prodVol).Run()
	})

	now := time.Now().UTC()
	specs := []struct {
		name string
		age  time.Duration
	}{
		{"pr-1", 10 * 24 * time.Hour},
		{"pr-2", 5 * 24 * time.Hour},
		{"pr-3", 1 * time.Hour},
	}

	for _, sp := range specs {
		dest := "preview-" + sp.name
		proj := release.ProjectName(appName, dest)
		vol := proj + "_data"
		ctr := proj + "-web-1"
		dir := release.AppDir(appName, dest)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(-sp.age)
		rec := deploy.PreviewRecord{
			App:            appName,
			Name:           sp.name,
			Destination:    dest,
			Project:        proj,
			Hosts:          []string{sp.name + ".preview.local"},
			Services:       []string{"web"},
			Version:        "v1",
			CreatedAt:      ts,
			LastDeployedAt: ts,
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "preview.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		dockerRun("volume", "create", "--label", "com.docker.compose.project="+proj, vol)
		dockerRun("run", "-d", "--name", ctr,
			"--label", "com.docker.compose.project="+proj,
			"--label", "yoho.app="+appName,
			"--label", "yoho.destination="+dest,
			"-v", vol+":/data",
			"busybox:latest", "sleep", "300")
		t.Cleanup(func() {
			exec.Command("docker", "rm", "-fv", ctr).Run()
			exec.Command("docker", "volume", "rm", "-f", vol).Run()
		})
	}

	g := &globals{configPath: filepath.Join(workDir, "yoho.yml")}
	c := previewCmd(g)
	var out strings.Builder
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"prune", "--keep", "1", "--yes"})
	if err := c.ExecuteContext(ctx); err != nil {
		t.Fatalf("preview prune failed: %v\n%s", err, out.String())
	}

	// Verify pr-1 and pr-2 were removed (App dir, container, volume).
	for _, removed := range []string{"pr-1", "pr-2"} {
		dest := "preview-" + removed
		proj := release.ProjectName(appName, dest)
		if _, err := os.Lstat(release.AppDir(appName, dest)); !os.IsNotExist(err) {
			t.Errorf("%s App dir still exists: %v", removed, err)
		}
		if err := exec.CommandContext(ctx, "docker", "container", "inspect", proj+"-web-1").Run(); err == nil {
			t.Errorf("%s container still exists", removed)
		}
		if err := exec.CommandContext(ctx, "docker", "volume", "inspect", proj+"_data").Run(); err == nil {
			t.Errorf("%s volume still exists", removed)
		}
	}

	// Verify newest preview (pr-3) remains intact.
	pr3Dest := "preview-pr-3"
	pr3Proj := release.ProjectName(appName, pr3Dest)
	if _, err := os.Stat(filepath.Join(release.AppDir(appName, pr3Dest), "preview.json")); err != nil {
		t.Errorf("newest preview pr-3 marker missing: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "container", "inspect", pr3Proj+"-web-1").Run(); err != nil {
		t.Errorf("newest preview pr-3 container missing: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "volume", "inspect", pr3Proj+"_data").Run(); err != nil {
		t.Errorf("newest preview pr-3 volume missing: %v", err)
	}

	// Verify non-preview production resources are untouched.
	if _, err := os.Stat(prodMarker); err != nil {
		t.Errorf("production App dir touched: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "container", "inspect", prodContainer).Run(); err != nil {
		t.Errorf("production container touched: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "volume", "inspect", prodVol).Run(); err != nil {
		t.Errorf("production volume touched: %v", err)
	}
}
