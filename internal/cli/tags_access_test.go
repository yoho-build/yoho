package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tag validation follows the resolved access (YOHO_ACCESS wins over the
// Yoho file), in both directions.
func TestComposeTagsUseResolvedAccess(t *testing.T) {
	for _, tc := range []struct {
		configured, env string
		refused         bool
	}{
		{"scoped", "operator", false},
		{"operator", "scoped", true},
		{"scoped", "", true},
		{"operator", "", false},
	} {
		t.Run(tc.configured+"/"+tc.env, func(t *testing.T) {
			t.Setenv("YOHO_ACCESS", tc.env)
			dir := t.TempDir()
			p := filepath.Join(dir, "yoho.yml")
			os.WriteFile(p, []byte("app: shop\nservers: {eu1: {ssh: deb@203.0.113.10, tags: [eu]}}\ndestinations:\n  production: {servers: [eu1], access: "+tc.configured+"}\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx\n    x-yoho:\n      tags: {eu: {env: {REGION: eu}}}\n"), 0o644)
			g := &globals{configPath: p}
			a, err := g.load(planCmd(g))
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.compose(context.Background())
			refused := err != nil && strings.Contains(err.Error(), "x-yoho.tags requires operator access")
			if refused != tc.refused {
				t.Fatalf("refused=%v, want %v: %v", refused, tc.refused, err)
			}
			if !tc.refused && err != nil {
				t.Fatal(err)
			}
		})
	}
}
