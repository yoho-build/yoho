package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
)

func nonemptyApp(t *testing.T, runtime, source, declaration, values string) string {
	t.Helper()
	p := appDir(t, runtime, source)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("secrets:\n  nonempty: [" + declaration + "]\n" + values + "\nproxy: {tunnel: {token_secret: KEY}}\nbackups:\n  targets:\n    local: {type: archive, repository: /tmp/yoho-test-backups, password_secret: KEY, env_secrets: [KEY]}\n  jobs:\n    nightly: {destination: production, target: local, schedule: '*-*-* 03:00:00'}\n")
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return p
}

func runNonemptyCommand(t *testing.T, g *globals, name string) (string, error) {
	t.Helper()
	var c *cobra.Command
	var args []string
	switch name {
	case "plan":
		c = planCmd(g)
		args = []string{"--version", "v1"}
	case "deploy":
		c = deployCmd(g)
		args = []string{"--version", "v1"}
	case "apply":
		c = applyCmd(g)
		args = []string{"--version", "v1", "--auto-approve"}
	case "config check":
		c = configCmd(g)
		args = []string{"check"}
	case "config show":
		c = configCmd(g)
		args = []string{"show"}
	case "secrets list":
		c = secretsCmd(g)
		args = []string{"list"}
	case "tunnel up":
		c = tunnelCmd(g)
		args = []string{"up"}
	case "schedule install":
		c = scheduleCmd(g)
		args = []string{"install"}
	default:
		c = backupCmd(g)
		args = []string{strings.TrimPrefix(name, "backup ")}
	}
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	c.SilenceErrors = true
	c.SilenceUsage = true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

func TestNonemptyFailsBeforeConnectionAndHooks(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	// A local fake ssh prevents any real Server access even if ordering regresses.
	bin := t.TempDir()
	marker := filepath.Join(bin, "ssh-called")
	t.Setenv("NONEMPTY_SSH_MARKER", marker)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf called > \"$NONEMPTY_SSH_MARKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	leaked := "private-provider-output"
	encoded := base64.StdEncoding.EncodeToString([]byte(leaked))
	for _, runtime := range []string{"compose", "swarm"} {
		for _, state := range []string{"missing", "empty", "provider-empty", "provider-error", "generated"} {
			for _, command := range []string{"deploy", "apply", "plan", "config check", "secrets list", "tunnel up", "backup run", "backup list", "backup check", "schedule install"} {
				t.Run(runtime+"/"+state+"/"+command, func(t *testing.T) {
					src := "services:\n  web:\n    image: nginx\n    volumes: [data:/data]\n    x-yoho:\n      stateful: true\n      secrets: {ALIAS: KEY}\n      backup: {volumes: [data]}\nvolumes: {data: {}}\n"
					if state == "missing" {
						src = strings.Replace(src, "      secrets: {ALIAS: KEY}\n", "", 1)
					}
					if state == "generated" {
						src = strings.Replace(src, "      secrets:", "      generate: {KEY: password32}\n      secrets:", 1)
					}
					values := ""
					if strings.HasPrefix(state, "provider-") {
						provider := filepath.Join(t.TempDir(), "fake-provider")
						script := "#!/bin/sh\nexit 0\n"
						if state == "provider-error" {
							script = fmt.Sprintf("#!/bin/sh\nprintf '%%s' '%s'\nprintf '%%s' '%s' >&2\nexit 1\n", leaked, encoded)
						}
						if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
							t.Fatal(err)
						}
						values = fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {KEY: {provider: fake, ref: opaque}}\n", provider)
					}
					p := nonemptyApp(t, runtime, src, "KEY", values)
					dir := filepath.Dir(p)
					if state == "empty" || state == "generated" {
						if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
							t.Fatal(err)
						}
						value := "KEY=\n"
						if state == "generated" {
							value = "KEY=local-shadow\n"
						}
						if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte(value), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					hookMarker := filepath.Join(dir, "hook-called")
					hooksDir := filepath.Join(dir, ".yoho", "hooks")
					if err := os.MkdirAll(hooksDir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(hooksDir, "pre-connect"), []byte("#!/bin/sh\nprintf called > '"+hookMarker+"'\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					out, err := runNonemptyCommand(t, &globals{configPath: p, json: true}, command)
					want := "secret KEY: required nonempty source value is missing or empty"
					if state == "provider-error" {
						want = "secret KEY: resolution failed"
					} else if state == "generated" {
						want = "source key KEY is generated on the Server"
					}
					if err == nil || !strings.Contains(out, want) || strings.Contains(out, "Connect") {
						t.Fatalf("error=%v output=%s", err, out)
					}
					if strings.Contains(out, leaked) || strings.Contains(out, encoded) || strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), encoded) {
						t.Fatal("provider output leaked")
					}
					for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
						var event map[string]any
						if err := json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatalf("invalid NDJSON: %q: %v", line, err)
						}
					}
					for _, m := range []string{marker, hookMarker} {
						if _, err := os.Stat(m); !os.IsNotExist(err) {
							t.Fatalf("operation before contract validation: %s (%v)", m, err)
						}
					}
				})
			}
		}
	}
}

func TestNonemptyLocalCheckAndCredentialDefaults(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	src := "services:\n  web: {image: nginx, x-yoho: {secrets: {ALIAS: KEY}}}\n"
	p := nonemptyApp(t, "compose", src, "KEY", "")
	dir := filepath.Dir(p)
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{" \t ", "first\nsecond\n"} {
		if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY='"+value+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := runNonemptyCommand(t, &globals{configPath: p, json: true}, "config check"); err != nil {
			t.Fatalf("valid value: %v, %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Config show doesn't resolve values, even under an opt-in declaration.
	if _, err := runNonemptyCommand(t, &globals{configPath: p}, "config show"); err != nil {
		t.Fatal(err)
	}
	c := configCmd(&globals{configPath: p})
	a, err := (&globals{configPath: p}).load(c)
	if err != nil {
		t.Fatal(err)
	}
	j, err := a.selectBackupJob("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.resolveTargetSecrets(context.Background(), j); err == nil {
		t.Fatal("empty Backup Target credential accepted")
	}
	a.cfg.Secrets.Nonempty = nil
	if err := a.resolveTargetSecrets(context.Background(), j); err != nil {
		t.Fatalf("default credential behavior changed: %v", err)
	}
	if j.password != "" || j.env["KEY"] != "" {
		t.Fatal("empty value changed")
	}
	store := secrets.New(map[string]string{"KEY": ""})
	if err := secretsFromStore(j, store); err != nil {
		t.Fatalf("retained credential behavior changed: %v", err)
	}
	// No credential path means no newly imposed local secret resolution.
	j.target.PasswordSecret = ""
	j.target.EnvSecrets = nil
	a.cfg.Secrets.Nonempty = []string{"MISSING"}
	if err := a.resolveTargetSecrets(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func TestNonemptyCommandFailureDiagnosticsAndFullConfigCheckScope(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	secret := "unknown/credential\"\nwith spaces"
	variants := []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), url.QueryEscape(secret), strconv.Quote(secret)}
	diagnostics := strings.Join(variants, " | ")
	for _, mode := range []string{"shared provider", "unrelated provider", "unrelated file command", "generated"} {
		for _, command := range []string{"config check", "secrets list", "plan", "apply"} {
			t.Run(mode+"/"+command, func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "provider-called")
				provider := filepath.Join(t.TempDir(), "fake-provider")
				script := "#!/bin/sh\nprintf called >> '" + marker + "'\nprintf '%s' '" + diagnostics + "' >&2\nexit 1\n"
				if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				src := "services:\n  web: {image: nginx, x-yoho: {secrets: {ALIAS: KEY}}}\n"
				values := fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {OTHER: {provider: fake, ref: shared}}\n", provider)
				if mode == "shared provider" {
					values = fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {KEY: {provider: fake, ref: shared}, OTHER: {provider: fake, ref: shared}}\n", provider)
				} else if mode == "unrelated file command" {
					values = ""
				} else if mode == "generated" {
					src = "services:\n  web: {image: nginx, x-yoho: {generate: {KEY: password32}, secrets: {ALIAS: KEY}}}\n"
				}
				p := nonemptyApp(t, "compose", src, "KEY", values)
				if mode != "shared provider" {
					dir := filepath.Join(filepath.Dir(p), ".yoho")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					file := "KEY=valid\n"
					if mode == "unrelated file command" {
						file += "OTHER=$('" + provider + "')\n"
					}
					if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte(file), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				out, err := runNonemptyCommand(t, &globals{configPath: p, json: true}, command)
				want := "secret OTHER: resolution failed"
				if mode == "generated" {
					want = "source key KEY is generated on the Server"
				}
				if err == nil || !strings.Contains(out, want) || strings.Contains(out, "Connect") {
					t.Fatalf("error=%v output=%s", err, out)
				}
				for _, variant := range variants {
					if strings.Contains(out, variant) || strings.Contains(err.Error(), variant) {
						t.Fatal("stderr-only credentials leaked")
					}
				}
				for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
					var event map[string]any
					if err := json.Unmarshal([]byte(line), &event); err != nil {
						t.Fatalf("invalid NDJSON: %q", line)
					}
				}
				calls, readErr := os.ReadFile(marker)
				if mode == "generated" {
					if !os.IsNotExist(readErr) {
						t.Fatalf("provider executed before generated-key rejection: %v", readErr)
					}
				} else if readErr != nil || string(calls) != "called" {
					t.Fatalf("expected one provider execution (including unrelated config-check resolution), got %q: %v", calls, readErr)
				}
			})
		}
	}
}

func TestNonemptyCredentiallessSchedulePreflight(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf called > \"$SCHEDULE_SSH_MARKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, compose, declaration string
		preflightFails             bool
	}{
		{"generated shadow", "services:\n  web: {image: nginx, x-yoho: {generate: {KEY: password32}, backup: {volumes: [data]}}}\n", "KEY", true},
		{"malformed compose", "services: [invalid\n", "KEY", true},
		{"credentialless valid contract", "services:\n  web: {image: nginx, x-yoho: {backup: {volumes: [data]}}}\n", "KEY", false},
		{"no contract default", "services:\n  web: {image: nginx, x-yoho: {backup: {volumes: [data]}}}\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providerMarker := filepath.Join(t.TempDir(), "provider-called")
			provider := filepath.Join(t.TempDir(), "fake-provider")
			if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf called > '"+providerMarker+"'\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			p := nonemptyApp(t, "compose", tc.compose, tc.declaration, fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {OTHER: {provider: fake, ref: opaque}}\n", provider))
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), ", password_secret: KEY, env_secrets: [KEY]", "", 1))
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(p)
			if err := os.MkdirAll(filepath.Join(dir, ".yoho", "hooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY=local-shadow\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			hookMarker := filepath.Join(dir, "hook-called")
			if err := os.WriteFile(filepath.Join(dir, ".yoho", "hooks", "pre-connect"), []byte("#!/bin/sh\nprintf called > '"+hookMarker+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			sshMarker := filepath.Join(dir, "ssh-called")
			t.Setenv("SCHEDULE_SSH_MARKER", sshMarker)
			out, err := runNonemptyCommand(t, &globals{configPath: p, json: true}, "schedule install")
			if err == nil {
				t.Fatal("expected preflight or fake SSH failure")
			}
			_, sshErr := os.Stat(sshMarker)
			if tc.preflightFails {
				if !os.IsNotExist(sshErr) || strings.Contains(out, "Connect") {
					t.Fatalf("SSH before local validation: %v, %s", sshErr, out)
				}
				if tc.name == "generated shadow" && !strings.Contains(out, "source key KEY is generated on the Server") {
					t.Fatalf("wrong preflight: %s", out)
				}
			} else if sshErr != nil || !strings.Contains(out, "Connect") {
				t.Fatalf("credentialless/default should reach fake SSH without resolution: %v %s", sshErr, out)
			}
			for _, marker := range []string{providerMarker, hookMarker} {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("unexpected provider or Hook call: %s", marker)
				}
			}
		})
	}
}

func TestNonemptyConfigCheckPreservesIndirectEnvironmentDependencies(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	bin := t.TempDir()
	provider := filepath.Join(bin, "fake-provider")
	helper := filepath.Join(bin, "indirect-helper")
	value := "indirect-provider-value"
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s' '"+value+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Neither the KEY expression nor the helper contains PROVIDER_SOURCE.
	// The helper derives its environment variable name from an earlier file key.
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nname=PROVIDER_$SELECTOR\neval \"printf '%s' \\\"\\${$name}\\\"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := nonemptyApp(t, "compose", "services:\n  web: {image: nginx, x-yoho: {secrets: {ALIAS: KEY}}}\n", "KEY", fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {PROVIDER_SOURCE: {provider: fake, ref: opaque}}\n", provider))
	dir := filepath.Join(filepath.Dir(p), ".yoho")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte("SELECTOR=SOURCE\nKEY=$('"+helper+"')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globals{configPath: p, json: true}
	if out, err := runNonemptyCommand(t, g, "config check"); err != nil || strings.Contains(out, value) {
		t.Fatalf("indirect dependency check: %v, %s", err, out)
	}
	a, err := g.load(configCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	store, err := a.loadSecrets(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := store.Get("KEY")
	if !ok || resolved != value {
		t.Fatal("indirect provider environment was not preserved")
	}
}

func TestConfigCheckWithoutNonemptyDoesNotRunProviders(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	marker := filepath.Join(t.TempDir(), "provider-called")
	provider := filepath.Join(t.TempDir(), "fake-provider")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf called > '"+marker+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := nonemptyApp(t, "compose", "services:\n  web: {image: nginx, x-yoho: {secrets: {ALIAS: KEY}}}\n", "", fmt.Sprintf("  providers: {fake: {type: command, command: [%q]}}\n  values: {KEY: {provider: fake, ref: opaque}}\n", provider))
	if out, err := runNonemptyCommand(t, &globals{configPath: p, json: true}, "config check"); err != nil {
		t.Fatalf("default check: %v %s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("provider called: %v", err)
	}
}
