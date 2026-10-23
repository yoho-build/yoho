package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

func TestCheckCloudflareSSH(t *testing.T) {
	dest := config.Destination{SSH: &config.DestinationSSH{Via: "cloudflare", Hostname: "ssh.example.com"}}
	withCF := loadProject(t, "services:\n  web: {image: nginx}\n  tunnel: {image: cloudflare/cloudflared:latest}\n")
	fs := CheckCloudflareSSH("production", dest, withCF.Project)
	if len(fs) != 1 || fs[0].Level != LevelWarning || fs[0].Service != "tunnel" {
		t.Fatalf("want one warning on tunnel, got %+v", fs)
	}
	if !strings.Contains(fs[0].Message, "ssh.example.com") || !strings.Contains(fs[0].Message, "tunnel") || !strings.Contains(fs[0].Message, "exit 255") {
		t.Fatalf("message: %s", fs[0].Message)
	}
	if !strings.Contains(fs[0].Message, "yoho releases") {
		t.Fatalf("missing outcome hint: %s", fs[0].Message)
	}

	named := loadProject(t, "services:\n  web: {image: nginx}\n  cloudflared: {image: alpine}\n")
	if fs := CheckCloudflareSSH("production", dest, named.Project); len(fs) != 1 || fs[0].Service != "cloudflared" {
		t.Fatalf("named cloudflared: %+v", fs)
	}

	plain := loadProject(t, "services:\n  web: {image: nginx}\n")
	if fs := CheckCloudflareSSH("production", dest, plain.Project); len(fs) != 0 {
		t.Fatalf("no connector: %+v", fs)
	}
	if fs := CheckCloudflareSSH("production", config.Destination{}, withCF.Project); len(fs) != 0 {
		t.Fatalf("no ssh: %+v", fs)
	}
}

func loadProject(t *testing.T, src string) *Result {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
