package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

func TestNonemptyResolvedValues(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
		valid            bool
	}{
		{"missing", "", "", false},
		{"empty", "KEY=\n", "", false},
		{"quoted empty", "KEY=''\n", "", false},
		{"whitespace", "KEY=' \t '\n", " \t ", true},
		{"newline only", "KEY='\n'\n", "\n", true},
		{"multiline", "KEY='first\nsecond\n'\n", "first\nsecond\n", true},
		{"variable empty", "OTHER=\nKEY=$OTHER\n", "", false},
		{"variable nonempty", "OTHER=value\nKEY=$OTHER\n", "value", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "secrets", tc.file)
			cfg := config.SecretsConfig{Nonempty: []string{"KEY"}}
			store, err := Load(context.Background(), dir, "production", cfg, LoadOptions{Getenv: noEnv})
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if v, _ := store.Get("KEY"); v != tc.want {
					t.Fatalf("changed value: %q", v)
				}
			} else if err == nil || err.Error() != "secret KEY: required nonempty source value is missing or empty" {
				t.Fatalf("error: %v", err)
			}
			cfg.Nonempty = nil
			if _, err := Load(context.Background(), dir, "production", cfg, LoadOptions{Getenv: noEnv}); err != nil {
				t.Fatalf("default changed: %v", err)
			}
		})
	}
}

func TestNonemptyProviderAndOverride(t *testing.T) {
	cfg := config.SecretsConfig{Nonempty: []string{"SOURCE"}, Providers: map[string]config.SecretProvider{"fake": {Type: "op"}}, Values: map[string]config.SecretValue{"SOURCE": {Provider: "fake", Ref: "opaque"}}}
	for _, value := range []string{"", " \t", "first\nsecond\n"} {
		t.Run(value, func(t *testing.T) {
			f := &fakeRunner{answers: map[string]fakeAnswer{"op read --no-newline opaque": {out: value}}}
			store, err := Load(context.Background(), t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv})
			if value == "" {
				if err == nil {
					t.Fatal("empty provider accepted")
				}
				defaultCfg := cfg
				defaultCfg.Nonempty = nil
				if _, err := Load(context.Background(), t.TempDir(), "production", defaultCfg, LoadOptions{Runner: f, Getenv: noEnv}); err != nil {
					t.Fatalf("default empty provider behavior changed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refs := config.SecretRefs{{Name: "CONTAINER_ALIAS", Key: "SOURCE"}}
			values, err := store.ForService(refs)
			if err != nil || values["CONTAINER_ALIAS"] != value {
				t.Fatalf("alias: %v, %v", values, err)
			}
		})
	}
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "SOURCE='valid'\n")
	writeFile(t, dir, "secrets.production", "SOURCE=\n")
	f := &fakeRunner{}
	if _, err := Load(context.Background(), dir, "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err == nil {
		t.Fatal("empty final override accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("overridden provider called: %v", f.calls)
	}
	writeFile(t, dir, "secrets.production", "SOURCE='valid'\n")
	if _, err := Load(context.Background(), dir, "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err != nil {
		t.Fatal(err)
	}
	cfg.Values["SOURCE"] = config.SecretValue{Provider: "fake", Ref: "opaque", Destinations: []string{"staging"}}
	if _, err := Load(context.Background(), t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err == nil {
		t.Fatal("destination-excluded declaration ignored")
	}
}

func TestNonemptyResolutionErrorsAreKeyOnly(t *testing.T) {
	secret := "credential-must-not-appear"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	cfg := config.SecretsConfig{Nonempty: []string{"KEY"}, Providers: map[string]config.SecretProvider{"fake": {Type: "command", Command: []string{"fake-provider"}}}, Values: map[string]config.SecretValue{"KEY": {Provider: "fake", Ref: secret}}}
	f := &fakeRunner{answers: map[string]fakeAnswer{"fake-provider " + secret: {out: secret, stderr: secret + encoded, err: errors.New(secret + encoded)}}}
	if _, err := Load(context.Background(), t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err == nil || err.Error() != "secret KEY: resolution failed" {
		t.Fatalf("error: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "KEY=$(fake-provider)\n")
	f = &fakeRunner{answers: map[string]fakeAnswer{"sh -c fake-provider": {out: secret, stderr: encoded, err: errors.New(secret)}}}
	if _, err := Load(context.Background(), dir, "production", config.SecretsConfig{Nonempty: []string{"KEY"}}, LoadOptions{Runner: f, Getenv: noEnv}); err == nil || err.Error() != "secret KEY: resolution failed" {
		t.Fatalf("error: %v", err)
	}
}

func TestNonemptySharedProviderFailureAndUndeclaredFileCommand(t *testing.T) {
	secret := "unknown/credential\"\nwith spaces"
	variants := []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), url.QueryEscape(secret), strconv.Quote(secret)}
	diagnostics := strings.Join(variants, " | ")
	cfg := config.SecretsConfig{
		Nonempty:  []string{"KEY"},
		Providers: map[string]config.SecretProvider{"fake": {Type: "command", Command: []string{"fake-provider"}}},
		Values: map[string]config.SecretValue{
			"KEY":   {Provider: "fake", Ref: "shared"},
			"OTHER": {Provider: "fake", Ref: "shared"},
		},
	}
	f := &fakeRunner{answers: map[string]fakeAnswer{"fake-provider shared": {stderr: diagnostics, err: errors.New("exit status 1")}}}
	_, err := Load(context.Background(), t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv})
	if err == nil || err.Error() != "secret KEY: resolution failed\nsecret OTHER: resolution failed" {
		t.Fatalf("shared failure: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("shared provider was not memoized: %v", f.calls)
	}
	for _, variant := range variants {
		if strings.Contains(err.Error(), variant) {
			t.Fatal("shared provider diagnostics leaked")
		}
	}
	cfg.Nonempty = nil
	if _, err := Load(context.Background(), t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err == nil || !strings.Contains(err.Error(), diagnostics) {
		t.Fatalf("default diagnostics changed: %v", err)
	}

	dir := t.TempDir()
	writeFile(t, dir, "secrets", "KEY=valid\nOTHER=$(fake-provider)\n")
	f = &fakeRunner{answers: map[string]fakeAnswer{"sh -c fake-provider": {stderr: diagnostics, err: errors.New("exit status 1")}}}
	if _, err := Load(context.Background(), dir, "production", config.SecretsConfig{Nonempty: []string{"KEY"}}, LoadOptions{Runner: f, Getenv: noEnv}); err == nil || err.Error() != "secret OTHER: resolution failed" {
		t.Fatalf("undeclared file command: %v", err)
	}
	if _, err := Load(context.Background(), dir, "production", config.SecretsConfig{}, LoadOptions{Runner: f, Getenv: noEnv}); err == nil || !strings.Contains(err.Error(), diagnostics) {
		t.Fatalf("default file diagnostics changed: %v", err)
	}
}

func TestNonemptyResolutionCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			cancel()
		}
		defer cancel()
		cfg := config.SecretsConfig{Nonempty: []string{"KEY"}, Providers: map[string]config.SecretProvider{"fake": {Type: "command", Command: []string{"fake"}}}, Values: map[string]config.SecretValue{"KEY": {Provider: "fake", Ref: "shared"}, "OTHER": {Provider: "fake", Ref: "shared"}}}
		f := &fakeRunner{answers: map[string]fakeAnswer{"fake shared": {stderr: "unknown-credential", err: want}}}
		if _, err := Load(ctx, t.TempDir(), "production", cfg, LoadOptions{Runner: f, Getenv: noEnv}); !errors.Is(err, want) || strings.Contains(err.Error(), "unknown-credential") {
			t.Fatalf("cancellation: %v", err)
		}
	}
}

func TestNonemptyAliasIsNotSourceAndMissingRefsUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "SOURCE=value\n")
	if _, err := Load(context.Background(), dir, "production", config.SecretsConfig{Nonempty: []string{"ALIAS"}}, LoadOptions{Getenv: noEnv}); err == nil || !strings.Contains(err.Error(), "secret ALIAS:") {
		t.Fatalf("alias declaration: %v", err)
	}
	store := New(map[string]string{"SOURCE": ""})
	if _, err := store.ForService(config.SecretRefs{{Name: "ALIAS", Key: "SOURCE"}}); err != nil {
		t.Fatalf("legitimate empty alias: %v", err)
	}
	if _, err := store.ForService(config.SecretRefs{{Name: "ALIAS", Key: "MISSING"}}); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing ref: %v", err)
	}
}
