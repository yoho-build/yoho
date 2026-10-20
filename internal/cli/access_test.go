package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
)

func ed25519Pub(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	for _, part := range [][]byte{[]byte("ssh-ed25519"), pub} {
		binary.Write(&blob, binary.BigEndian, uint32(len(part)))
		blob.Write(part)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob.Bytes()) + " alice@laptop\n"
}

func TestParsePublicKeyAndLine(t *testing.T) {
	key := ed25519Pub(t)
	typ, blob, fp, err := parsePublicKey([]byte(key))
	if err != nil || typ != "ssh-ed25519" || !strings.HasPrefix(fp, "SHA256:") || strings.HasSuffix(fp, "=") {
		t.Fatalf("%s %s %v", typ, fp, err)
	}
	line := gateKeyLine("alice", typ, blob, fp)
	want := `restrict,command="sudo -n /usr/local/bin/yoho gate --member alice --key ` + fp + `" ssh-ed25519 ` + blob + " yoho-gate:alice"
	if line != want {
		t.Fatalf("line\n%s\nwant\n%s", line, want)
	}
	if m := gateLineRe.FindStringSubmatch(line); m == nil || m[1] != "alice" || m[2] != fp {
		t.Fatalf("line not recognized: %v", m)
	}
	if strings.Contains(line, "--root") {
		t.Fatalf("the root must not be settable from argv: %s", line)
	}
	for name, bad := range map[string]string{
		"options":   `command="sh" ` + key,
		"two keys":  key + key,
		"garbage":   "ssh-ed25519 !!!",
		"mismatch":  "ssh-rsa " + strings.Fields(key)[1],
		"dsa":       "ssh-dss AAAA",
		"empty":     "",
		"truncated": "ssh-ed25519 AAAA",
	} {
		if _, _, _, err := parsePublicKey([]byte(bad)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestSudoersRule(t *testing.T) {
	r := sudoersRule("yoho-gate")
	for _, want := range []string{"yoho-gate ALL=(root) NOPASSWD: /usr/local/bin/yoho gate *\n", "!use_pty", "env_reset", `secure_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"`, `env_keep = "SSH_CONNECTION"`} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in\n%s", want, r)
		}
	}
	if strings.Contains(filepath.Base(sudoersPath), ".") {
		t.Fatal("sudo skips sudoers.d files with a dot")
	}
}

func TestScopedAccessOverride(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if s, _ := scopedAccess(config.Destination{Access: "scoped"}, get); !s {
		t.Fatal("scoped")
	}
	env["YOHO_ACCESS"] = "operator"
	if s, _ := scopedAccess(config.Destination{Access: "scoped"}, get); s {
		t.Fatal("YOHO_ACCESS=operator must win")
	}
	env["YOHO_ACCESS"] = "scoped"
	if s, _ := scopedAccess(config.Destination{}, get); !s {
		t.Fatal("YOHO_ACCESS=scoped")
	}
	env["YOHO_ACCESS"] = "root"
	if _, err := scopedAccess(config.Destination{}, get); err == nil {
		t.Fatal("want error")
	}
}

// scopedApp writes a Yoho file whose production Destination is scoped.
func scopedApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "yoho.yml"), []byte("app: shop\nservers: {primary: {ssh: deb@203.0.113.10}}\ndestinations:\n  production: {servers: [primary], access: scoped}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {web: {image: nginx}}\n"), 0o644)
	return filepath.Join(dir, "yoho.yml")
}

func TestScopedRefusesOperatorCommands(t *testing.T) {
	t.Setenv("YOHO_ACCESS", "")
	path := scopedApp(t)
	g := &globals{configPath: path}
	root := &cobra.Command{Use: "yoho", SilenceUsage: true, SilenceErrors: true}
	for _, add := range []func(*globals) *cobra.Command{appCmds, proxyCmd, tunnelCmd, scheduleCmd} {
		root.AddCommand(add(g))
	}
	for _, args := range [][]string{{"app", "exec", "web", "--", "id"}, {"proxy", "boot"}, {"tunnel", "up"}, {"schedule", "status"}} {
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		var silent *silentError
		if !errors.As(err, &silent) || !strings.Contains(out.String(), "not available with scoped access") || !strings.Contains(out.String(), "ask the operator") {
			t.Errorf("%v: err %v out %q", args, err, out.String())
		}
	}
	for _, c := range [][]string{{"deploy"}, {"app", "logs"}, {"tui"}, {"releases"}} {
		cmd := &cobra.Command{Use: c[len(c)-1]}
		parent := root
		if len(c) == 2 {
			parent = &cobra.Command{Use: c[0]}
			root.AddCommand(parent)
		}
		parent.AddCommand(cmd)
		if !scopedCommand(cmd) {
			t.Errorf("%v should be served through the Gate", c)
		}
	}
}

func TestLoadGateRoot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gate.json")
	if r, err := loadGateRoot(p); err != nil || r != release.DefaultRoot {
		t.Fatalf("missing: %q %v", r, err)
	}
	os.WriteFile(p, []byte(`{"root":"/home/deb/yoho"}`), 0o644)
	if r, err := loadGateRoot(p); err != nil || r != "/home/deb/yoho" {
		t.Fatalf("root: %q %v", r, err)
	}
	os.Chmod(p, 0o666)
	if _, err := loadGateRoot(p); err == nil {
		t.Fatal("want error for a world-writable config")
	}
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte(`{"root":"relative/../x"}`), 0o644)
	if _, err := loadGateRoot(p); err == nil {
		t.Fatal("want error for a relative root")
	}
}
