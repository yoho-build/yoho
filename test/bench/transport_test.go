package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real benchmark control flow without Docker, SSH, or large data.
func TestTransportScript(t *testing.T) {
	for _, tc := range []struct{ name, modes, goos, status, want, calls string }{
		{"archive baselines", "gzip zstd", "Linux", "0", "zstd,warm,0,0,compress-transfer-load,OK", "docker build --load --platform"},
		{"Linux container", "container", "Linux", "0", "SKIP: Apple container unavailable", ""},
		{"stopped container", "container", "Darwin", "1", "SKIP: Apple container unavailable", "container system status"},
		{"running container", "container", "Darwin", "0", "container,warm,0,0,deploy-including-build,OK", "yoho --json deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
set -eu
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$CALL_LOG"
case "$name" in
 uname) echo "$FAKE_OS";;
 container) exit "$CONTAINER_STATUS";;
 date) echo 100;;
 dd) for arg do case "$arg" in of=*) echo data > "${arg#of=}";; esac; done;;
 docker)
  case "$1" in save) while [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; echo archive > "$1"; break; fi; shift; done;; esac;;
 ssh) case "$*" in *rx_bytes*) echo 100;; esac;;
 gzip|zstd) for arg do last=$arg; done; cat "$last";;
esac
`
			for _, name := range []string{"uname", "container", "date", "dd", "docker", "ssh", "gzip", "zstd", "yoho"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(dir, "calls")
			output := filepath.Join(dir, "results")
			cmd := exec.Command("sh", "transport.sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CALL_LOG="+log,
				"FAKE_OS="+tc.goos, "CONTAINER_STATUS="+tc.status, "BENCH_SSH=disposable", "BENCH_INTERFACE=eth0",
				"BENCH_MODES="+tc.modes, "BENCH_OUTPUT="+output, "YOHO="+filepath.Join(bin, "yoho"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			csv, err := os.ReadFile(filepath.Join(output, "results.csv"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(csv), tc.want) {
				t.Fatal(string(csv))
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), tc.calls) {
				t.Fatal(string(calls))
			}
			if tc.name == "archive baselines" && !strings.Contains(string(calls), "set -eu; zstd -dc | docker load") {
				t.Fatal(string(calls))
			}
			if strings.Contains(tc.want, "SKIP") && strings.Contains(string(calls), "yoho --json deploy") {
				t.Fatal("attempted unavailable container deploy")
			}
			if tc.goos == "Linux" && strings.Contains(string(calls), "container system status") {
				t.Fatal("probed Apple container on Linux")
			}
		})
	}
}
