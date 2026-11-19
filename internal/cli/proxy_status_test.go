package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// A stopped Proxy cannot list routes; status still prints its summary
// instead of failing on `docker exec`.
func TestPrintProxyStatusStoppedProxy(t *testing.T) {
	bin := t.TempDir()
	docker := `#!/bin/sh
case "$1 $2" in
"ps -a") printf 'yoho-proxy\tExited (0) 1 minute ago\t\n' ;;
"ps -q"*|"ps --filter"*) ;;
"exec "*) echo "container yoho-proxy is not running" >&2; exit 1 ;;
"container inspect") echo 'abc|false|basecamp/kamal-proxy:v0.10.2' ;;
*) case "$*" in *"ps -q"*) ;; *) echo "unexpected docker $*" >&2; exit 2 ;; esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	if err := printProxyStatus(context.Background(), &remote.Local{}, config.ProxyConfig{}, &out, &errOut); err != nil {
		t.Fatalf("err=%v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "Exited") || !strings.Contains(out.String(), "proxy version: v0.10.2") {
		t.Fatalf("out=%q", out.String())
	}
}
