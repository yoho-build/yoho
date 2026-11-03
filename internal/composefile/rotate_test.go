package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRotate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`
services:
  db:
    image: postgres:17
    restart: unless-stopped
    x-yoho:
      stateful: true
      generate: {POSTGRES_PASSWORD: password32, PEPPER: hex32, A: hex32, B: hex32}
      rotate:
        POSTGRES_PASSWORD: {strategy: postgres, user: app}
        PEPPER: {strategy: command}
        A: {strategy: rot13}
        B: {strategy: none, user: x}
        MISSING: {strategy: none}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	errs := msgs(Check(r, "compose", nil), LevelError)
	for _, want := range []string{
		"x-yoho.rotate.PEPPER: strategy command needs a command",
		`x-yoho.rotate.A: unknown strategy "rot13"`,
		"x-yoho.rotate.B: strategy none takes no user or command",
		"x-yoho.rotate.MISSING: not in this Service's x-yoho.generate",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, "rotate.POSTGRES_PASSWORD") {
		t.Errorf("valid entry rejected:\n%s", errs)
	}
}
