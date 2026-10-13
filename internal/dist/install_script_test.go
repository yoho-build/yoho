package dist

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This harness exercises POSIX shell control flow with real hashing and isolated
// destinations. Fake gh tests policy arguments/failure handling, not signatures.
func TestInstallerProvenance(t *testing.T) {
	type installCase struct {
		name, mode, version, ghMode, latest, token, ghToken, osName, arch, apiHost string
		noGH, badChecksum, existing, success                                       bool
		wantError                                                                  string
	}
	cases := []installCase{
		{name: "default pinned no gh", version: "v1.2.3", noGH: true, success: true},
		{name: "default latest no gh", noGH: true, success: true},
		{name: "zero pinned no gh", mode: "0", version: "v1.2.3", noGH: true, success: true},
		{name: "zero latest no gh", mode: "0", noGH: true, success: true},
		{name: "private pinned default", version: "v1.2.3", token: "github-secret", noGH: true, success: true},
		{name: "private latest default", ghToken: "gh-secret", noGH: true, success: true},
		{name: "private pinned zero", mode: "0", version: "v1.2.3", token: "github-secret", noGH: true, success: true},
		{name: "private latest zero", mode: "0", token: "github-secret", noGH: true, success: true},
		{name: "required pinned", mode: "1", version: "v1.2.3", success: true},
		{name: "required latest", mode: "1", latest: "v1.2.3", success: true},
		{name: "required private pinned token precedence", mode: "1", version: "v1.2.3", token: "github-secret", ghToken: "gh-secret", success: true},
		{name: "required private latest token precedence", mode: "1", latest: "v1.2.3", token: "github-secret", ghToken: "gh-secret", success: true},
		{name: "required gh token", mode: "1", version: "v1.2.3", ghToken: "gh-secret", success: true},
		{name: "default ignores custom API host", version: "v1.2.3", apiHost: "attacker.invalid", success: true},
		{name: "zero ignores custom API host", mode: "0", version: "v1.2.3", apiHost: "attacker.invalid", success: true},
		{name: "required canonical API host pinned", mode: "1", version: "v1.2.3", apiHost: "api.github.com", success: true},
		{name: "required canonical API host latest", mode: "1", latest: "v1.2.3", apiHost: "api.github.com", token: "github-secret", success: true},
		{name: "required custom API host pinned", mode: "1", version: "v1.2.3", apiHost: "attacker.invalid", token: "github-secret", existing: true, wantError: "refuses a custom GitHub CLI API host"},
		{name: "required custom API host latest", mode: "1", apiHost: "attacker.invalid", token: "github-secret", existing: true, wantError: "refuses a custom GitHub CLI API host"},
		{name: "required config failure pinned", mode: "1", version: "v1.2.3", ghMode: "config-fail", token: "github-secret", existing: true, wantError: "cannot check GitHub CLI API host"},
		{name: "required config failure latest", mode: "1", ghMode: "config-fail", token: "github-secret", existing: true, wantError: "cannot check GitHub CLI API host"},
		{name: "required missing gh", mode: "1", version: "v1.2.3", noGH: true, wantError: "requires GitHub CLI"},
		{name: "invalid switch", mode: "yes", noGH: true, wantError: "must be 0 or 1"},
		{name: "empty switch", mode: "<empty>", noGH: true, wantError: "must be 0 or 1"},
		{name: "required checksum mismatch", mode: "1", version: "v1.2.3", badChecksum: true, existing: true, wantError: "checksum mismatch"},
		{name: "default checksum mismatch", version: "v1.2.3", badChecksum: true, wantError: "checksum mismatch"},
		{name: "required latest resolution network", mode: "1", ghMode: "api-fail", wantError: "cannot resolve latest"},
		{name: "latest empty", mode: "1", latest: "<empty>", wantError: "invalid release tag"},
		{name: "latest null", mode: "1", latest: "null", wantError: "invalid release tag"},
		{name: "latest option injection", mode: "1", latest: "--help", wantError: "invalid release tag"},
		{name: "latest whitespace", mode: "1", latest: "v1.2.3\nv9.9.9", wantError: "invalid release tag"},
		{name: "latest slash", mode: "1", latest: "v1/evil", wantError: "invalid release tag"},
		{name: "latest traversal", mode: "1", latest: "v1..2", wantError: "invalid release tag"},
		{name: "latest trailing dot", mode: "1", latest: "v1.", wantError: "invalid release tag"},
		{name: "pinned option injection", mode: "1", version: "--help", wantError: "invalid release tag"},
		{name: "unsupported OS", mode: "1", version: "v1.2.3", osName: "Windows", wantError: "unsupported OS"},
		{name: "unsupported architecture", mode: "1", version: "v1.2.3", arch: "riscv64", wantError: "unsupported architecture"},
		{name: "Darwin arm64", mode: "1", version: "v1.2.3", osName: "Darwin", arch: "arm64", success: true},
		{name: "Linux aarch64", mode: "1", version: "v1.2.3", arch: "aarch64", success: true},
	}
	for _, failure := range []string{"old-gh", "unsupported-flag", "auth", "network", "no-attestation", "wrong-digest", "wrong-repo", "wrong-workflow", "wrong-tag"} {
		cases = append(cases, installCase{name: failure, mode: "1", version: "v1.2.3", ghMode: failure, existing: true, token: "github-secret", wantError: "provenance verification failed"})
	}
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "install.sh")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "tools")
			tmp := filepath.Join(root, "temporary files")
			fixtures := filepath.Join(root, "fixtures")
			dest := filepath.Join(root, "install space's", "nested", "bin")
			for _, dir := range []string{bin, tmp, fixtures} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// A strict utility allowlist: no real curl, wget or gh can be reached.
			for _, tool := range []string{"awk", "rm", "chmod", "mkdir", "cp", "dirname"} {
				linkInstallerTool(t, bin, tool)
			}
			// macOS mktemp without a template can ignore TMPDIR; force a real
			// unique directory inside the harness so cleanup assertions are real.
			mktemp, err := exec.LookPath("mktemp")
			if err != nil {
				t.Fatal(err)
			}
			writeInstallerFile(t, filepath.Join(bin, "mktemp"), "#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = -d ] || exit 1\nexec \"$REAL_MKTEMP\" -d \"$TMPDIR/download.XXXXXXXX\"\n", 0o755)
			hashTool := "sha256sum"
			if _, err := exec.LookPath(hashTool); err != nil {
				hashTool = "shasum"
			}
			linkInstallerTool(t, bin, hashTool)
			writeInstallerFile(t, filepath.Join(bin, "uname"), "#!/bin/sh\ncase $1 in -s) printf '%s\\n' \"$FAKE_OS\";; -m) printf '%s\\n' \"$FAKE_ARCH\";; *) exit 1;; esac\n", 0o755)
			writeInstallerFile(t, filepath.Join(bin, "curl"), fakeInstallerCurl, 0o755)
			if !tc.noGH {
				writeInstallerFile(t, filepath.Join(bin, "gh"), fakeInstallerGH, 0o755)
			}
			payload := "#!/bin/sh\nif [ \"$REQUIRE_VERIFY\" = 1 ] && [ ! -f \"$VERIFIED\" ]; then exit 88; fi\nprintf 'executed\\n' >> \"$EXEC_MARKER\"\nprintf 'yoho v1.2.3\\n'\n"
			writeInstallerFile(t, filepath.Join(fixtures, "binary"), payload, 0o600)
			sum := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
			if tc.badChecksum {
				sum = strings.Repeat("0", 64)
			}
			goos, goarch := "linux", "amd64"
			osName, arch := tc.osName, tc.arch
			if osName == "" {
				osName = "Linux"
			}
			if arch == "" {
				arch = "x86_64"
			}
			if osName == "Darwin" {
				goos = "darwin"
			}
			if arch == "arm64" || arch == "aarch64" {
				goarch = "arm64"
			}
			asset := "yoho-" + goos + "-" + goarch
			writeInstallerFile(t, filepath.Join(fixtures, "checksums"), sum+"  "+asset+"\n", 0o600)
			writeInstallerFile(t, filepath.Join(fixtures, "release"), fmt.Sprintf("{\n  \"assets\": [\n    {\n      \"url\": \"https://api.github.com/repos/yoho-build/yoho/releases/assets/1\",\n      \"name\": \"%s\"\n    },\n    {\n      \"url\": \"https://api.github.com/repos/yoho-build/yoho/releases/assets/2\",\n      \"name\": \"checksums.txt\"\n    }\n  ]\n}\n", asset), 0o600)
			if tc.existing {
				writeInstallerFile(t, filepath.Join(dest, "yoho"), "old installed binary\n", 0o755)
			}
			latest := tc.latest
			if latest == "" {
				latest = "v1.2.3"
			}
			if latest == "<empty>" {
				latest = ""
			}
			expectedToken := tc.token
			if expectedToken == "" {
				expectedToken = tc.ghToken
			}
			cmd := exec.Command("/bin/sh", script)
			cmd.Env = []string{
				"PATH=" + bin, "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + tmp, "REAL_MKTEMP=" + mktemp,
				"YOHO_INSTALL_DIR=" + dest, "YOHO_VERSION=" + tc.version,
				"GITHUB_TOKEN=" + tc.token, "GH_TOKEN=" + tc.ghToken, "GH_HOST=untrusted.invalid",
				"FIXTURES=" + fixtures, "CURL_LOG=" + filepath.Join(root, "curl.log"),
				"GH_LOG=" + filepath.Join(root, "gh.log"), "GH_MODE=" + tc.ghMode,
				"LATEST_TAG=" + latest, "EXPECTED_TOKEN=" + expectedToken, "FAKE_API_HOST=" + tc.apiHost,
				"FAKE_OS=" + osName, "FAKE_ARCH=" + arch, "ASSET=" + asset,
				"VERIFIED=" + filepath.Join(root, "verified"), "EXEC_MARKER=" + filepath.Join(root, "executed"),
				"REQUIRE_VERIFY=" + tc.mode,
			}
			if tc.mode != "" {
				mode := tc.mode
				if mode == "<empty>" {
					mode = ""
				}
				cmd.Env = append(cmd.Env, "YOHO_VERIFY_PROVENANCE="+mode)
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("success=%v, err=%v\n%s", tc.success, err, out)
			}
			if tc.wantError != "" && !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("want %q, got %s", tc.wantError, out)
			}
			ghLog := readInstallerLog(t, filepath.Join(root, "gh.log"))
			curlLog := readInstallerLog(t, filepath.Join(root, "curl.log"))
			for _, secret := range []string{tc.token, tc.ghToken} {
				if secret != "" && strings.Contains(string(out)+ghLog+curlLog, secret) {
					t.Fatal("secret leaked in output or argv")
				}
			}
			_, execErr := os.Stat(filepath.Join(root, "executed"))
			if tc.success && execErr != nil {
				t.Fatal("binary was not executed")
			}
			if !tc.success && !os.IsNotExist(execErr) {
				t.Fatal("failed install executed binary")
			}
			installed, installErr := os.ReadFile(filepath.Join(dest, "yoho"))
			if tc.success && (installErr != nil || string(installed) != payload) {
				t.Fatal("installed binary differs")
			}
			if !tc.success {
				if tc.existing {
					if installErr != nil || string(installed) != "old installed binary\n" {
						t.Fatal("failed verification modified existing binary")
					}
				} else if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failure touched destination")
				}
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files not cleaned: %v %v", entries, err)
			}
			if tc.mode != "1" && ghLog != "" {
				t.Fatal("checksum-only mode invoked gh")
			}
			if tc.badChecksum && (strings.Contains(ghLog, "attestation\n") || strings.Contains(ghLog, "api\n")) {
				t.Fatal("checksum mismatch invoked gh networking or attestation verification")
			}
			if strings.Contains(tc.wantError, "GitHub CLI API host") {
				if curlLog != "" || strings.Contains(ghLog, "api\n") || strings.Contains(ghLog, "attestation\n") {
					t.Fatal("unsafe or unreadable API host reached networking")
				}
			}
			if tc.wantError == "must be 0 or 1" || tc.wantError == "requires GitHub CLI" || tc.wantError == "invalid release tag" {
				if curlLog != "" {
					t.Fatal("preflight failure downloaded files")
				}
			}
			if tc.success && tc.mode == "1" {
				for _, arg := range []string{"attestation\nverify\n", "--hostname\ngithub.com\n", "--repo\nyoho-build/yoho\n", "--digest-alg\nsha256\n", "--cert-identity\nhttps://github.com/yoho-build/yoho/.github/workflows/release.yml@refs/tags/v1.2.3\n", "--source-ref\nrefs/tags/v1.2.3\n", "--predicate-type\nhttps://slsa.dev/provenance/v1\n", "--cert-oidc-issuer\nhttps://token.actions.githubusercontent.com\n"} {
					if !strings.Contains(ghLog, arg) {
						t.Errorf("missing policy %q in %s", arg, ghLog)
					}
				}
				if !strings.Contains(ghLog, tmp+"/") || !strings.Contains(ghLog, "/"+asset+"\n") {
					t.Fatalf("gh did not receive downloaded binary under %s: %s", tmp, ghLog)
				}
				if tc.version == "" {
					if strings.Count(ghLog, "repos/yoho-build/yoho/releases/latest\n") != 1 {
						t.Fatal("latest not resolved exactly once")
					}
					if strings.Contains(curlLog, "/latest") {
						t.Fatal("downloads used moving latest")
					}
					if !strings.Contains(curlLog, "/v1.2.3") {
						t.Fatal("downloads did not use resolved tag")
					}
				} else if strings.Contains(ghLog, "api\n") {
					t.Fatal("pinned version unnecessarily resolved latest")
				}
			}
			if tc.success && tc.mode != "1" && tc.version == "" && !strings.Contains(curlLog, "/latest") {
				t.Fatal("legacy latest changed")
			}
		})
	}
}

func linkInstallerTool(t *testing.T, dir, name string) {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func writeInstallerFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func readInstallerLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const fakeInstallerCurl = `#!/bin/sh
set -eu
if [ "$1" = --version ]; then printf 'curl 8.0.0 test\n'; exit 0; fi
printf 'CALL\n' >> "$CURL_LOG"
printf '%s\n' "$@" >> "$CURL_LOG"
output= config= url=
while [ "$#" -gt 0 ]; do
 case "$1" in
 -o) output=$2; shift 2;;
 --config) config=$2; shift 2;;
 -H) shift 2;;
 -*) shift;;
 *) url=$1; shift;;
 esac
done
if [ -n "$EXPECTED_TOKEN" ]; then
 [ -n "$config" ] || exit 90
 IFS= read -r header < "$config"
 [ "$header" = "header = \"Authorization: Bearer $EXPECTED_TOKEN\"" ] || exit 91
fi
case "$url" in
 https://github.com/yoho-build/yoho/releases/*/checksums.txt|https://api.github.com/repos/yoho-build/yoho/releases/assets/2) cp "$FIXTURES/checksums" "$output";;
 https://github.com/yoho-build/yoho/releases/*/yoho-*|https://api.github.com/repos/yoho-build/yoho/releases/assets/1) cp "$FIXTURES/binary" "$output";;
 https://api.github.com/repos/yoho-build/yoho/releases/latest|https://api.github.com/repos/yoho-build/yoho/releases/tags/*) cp "$FIXTURES/release" "$output";;
 *) exit 92;;
esac
`

const fakeInstallerGH = `#!/bin/sh
set -eu
printf 'CALL\n' >> "$GH_LOG"
printf '%s\n' "$@" >> "$GH_LOG"
[ "$GH_HOST" = github.com ] || exit 93
[ "${GH_TOKEN:-}" = "$EXPECTED_TOKEN" ] || exit 94
case "$1" in
 config)
  [ "$2" = get ] && [ "$3" = api_host ] && [ "$4" = --host ] && [ "$5" = github.com ] || exit 98
  if [ "$GH_MODE" = config-fail ]; then printf '%s\n' "${GH_TOKEN:-}" >&2; exit 1; fi
  printf '%s\n' "$FAKE_API_HOST";;
 api)
  [ "$2" = --hostname ] && [ "$3" = github.com ] && [ "$4" = repos/yoho-build/yoho/releases/latest ] && [ "$5" = --jq ] && [ "$6" = .tag_name ] || exit 95
  if [ "$GH_MODE" = api-fail ]; then printf '%s\n' "$GH_TOKEN" >&2; exit 1; fi
  printf '%s\n' "$LATEST_TAG";;
 attestation)
  [ "$2" = verify ] && [ -f "$3" ] || exit 96
  if [ -n "$GH_MODE" ]; then printf '%s: %s\n' "$GH_MODE" "${GH_TOKEN:-}" >&2; exit 1; fi
  printf 'verified\n' > "$VERIFIED";;
 *) exit 97;;
esac
`

// A static contract regression, not a GitHub server-side Actions validator.
func TestReleaseAttestationWorkflow(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{"contents: write", "id-token: write", "attestations: write", "go-version: '1.26.x'", "check-latest: true", "actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8", "subject-path: dist/yoho-*", "create-storage-record: false"} {
		if !strings.Contains(text, required) {
			t.Errorf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"artifact-metadata:", "${{ runner.", "go-version-file:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unexpected %s", forbidden)
		}
	}
	last := -1
	for _, stage := range []string{"make dist", "make checksums", "Attest published binaries", "Publish GitHub Release"} {
		pos := strings.Index(text, stage)
		if pos <= last {
			t.Fatalf("incorrect workflow order: %s", stage)
		}
		last = pos
	}
}
