package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "web.env"), []byte("FROM_FILE=1\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`services:
  web:
    image: yoho/shop-production-web:abc
    env_file: [web.env]
    environment: {PRICE: "$$5"}
    volumes: [data:/data]
    x-yoho: {proxy: {hosts: [shop.example.com]}, secrets: [DB_URL]}
volumes: {data: {}}
`), 0o600)
	r, err := Load(context.Background(), dir, nil, nil, "yoho-shop-production", "production")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Resolved(r.Project)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "env_file") {
		t.Fatalf("env_file kept:\n%s", raw)
	}
	back, err := LoadResolved(context.Background(), raw, t.TempDir(), "yoho-shop-production")
	if err != nil {
		t.Fatal(err)
	}
	web := back.Project.Services["web"]
	if *web.Environment["FROM_FILE"] != "1" || *web.Environment["PRICE"] != "$5" {
		t.Fatalf("environment %v", web.Environment)
	}
	if back.Project.Volumes["data"].Name != "yoho-shop-production_data" {
		t.Fatalf("volume %+v", back.Project.Volumes["data"])
	}
	if ext := back.Ext["web"]; ext.Proxy == nil || ext.Proxy.Hosts[0] != "shop.example.com" || ext.Secrets[0].Key != "DB_URL" {
		t.Fatalf("x-yoho %+v", ext)
	}
}
