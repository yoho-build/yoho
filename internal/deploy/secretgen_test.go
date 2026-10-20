package deploy

import (
	"context"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestParseEnvFileRoundTrip(t *testing.T) {
	m := map[string]string{"A": `quote " back \ dollar $X`, "B": "line1\nline2\r\n", "C": ""}
	got, err := parseEnvFile(envFile(m))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range m {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	if _, err := parseEnvFile([]byte("A=unquoted\n")); err == nil {
		t.Fatal("want error for a foreign env file")
	}
}

func TestSecretsGenerationRoundTrip(t *testing.T) {
	old := release.Root
	release.Root = t.TempDir()
	defer func() { release.Root = old }()
	ctx := context.Background()
	h := &remote.Local{}
	in := map[string]map[string]string{"web": {"DB_URL": "postgres://x", "K": "v\n"}, "worker": {"TOKEN": "t$1"}}
	gen, err := WriteSecretsGeneration(ctx, h, "shop", "production", "access-push", in, map[string]bool{"worker": true})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidGeneration(gen) || ValidGeneration("../x") {
		t.Fatalf("ValidGeneration(%q)", gen)
	}
	got, err := ReadSecretsGeneration(release.SecretsDir("shop", "production", gen))
	if err != nil {
		t.Fatal(err)
	}
	for svc, m := range in {
		for k, v := range m {
			if got[svc][k] != v {
				t.Errorf("%s/%s: got %q want %q", svc, k, got[svc][k], v)
			}
		}
	}
	if _, err := WriteSecretsGeneration(ctx, h, "shop", "production", "../x", nil, nil); err == nil {
		t.Fatal("want error for a bad suffix")
	}
}
