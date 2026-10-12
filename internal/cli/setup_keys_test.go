package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveAuthorizedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	local := "ssh-ed25519 AAAAC3Nz op@laptop"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id.pub"), []byte(local+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveAuthorizedKeys(nil); !reflect.DeepEqual(got, []string{local}) {
		t.Errorf("unset: %v", got)
	}
	if got := resolveAuthorizedKeys([]string{}); len(got) != 0 {
		t.Errorf("explicit empty: %v", got)
	}
	if got := resolveAuthorizedKeys([]string{"ssh-rsa X"}); !reflect.DeepEqual(got, []string{"ssh-rsa X"}) {
		t.Errorf("explicit: %v", got)
	}
}
