package deploy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// ChangedServices returns the sorted Service names whose secret values
// differ between prev and next. Maps are Service -> container name -> value.
// The result contains names only; values are never copied into it.
func ChangedServices(prev, next map[string]map[string]string) []string {
	seen := map[string]bool{}
	for name := range prev {
		seen[name] = true
	}
	for name := range next {
		seen[name] = true
	}
	var out []string
	for _, name := range sortedKeys(seen) {
		if !sameSecretMap(prev[name], next[name]) {
			out = append(out, name)
		}
	}
	return out
}

func sameSecretMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ChangedServicesFromAudit compares Release secret audits (fingerprints,
// never values) and returns the Services whose set changed.
func ChangedServicesFromAudit(prev, next map[string]release.SecretAudit) []string {
	return ChangedServices(auditFingerprints(prev), auditFingerprints(next))
}

func auditFingerprints(a map[string]release.SecretAudit) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, v := range a {
		svc, name, ok := strings.Cut(k, "/")
		if !ok || svc == "" || name == "" {
			continue
		}
		if out[svc] == nil {
			out[svc] = map[string]string{}
		}
		out[svc][name] = v.Fingerprint
	}
	return out
}

// DetectChangedServices compares the current Release's secret audit with
// the secrets this Deploy would write (locally resolved values plus
// generated files already on the Server). It never returns secret values.
func DetectChangedServices(ctx context.Context, d *plan.Deploy, cur *release.Release) ([]string, error) {
	if d == nil || cur == nil {
		return nil, fmt.Errorf("deploy and current Release are required")
	}
	if len(d.Servers) == 0 || d.Servers[0].Host == nil {
		return nil, fmt.Errorf("no Server")
	}
	h := d.Servers[0].Host
	dir := release.AppDir(d.App, d.Destination)
	kinds, err := generatedDecls(d)
	if err != nil {
		return nil, err
	}
	generated := map[string]string{}
	for _, name := range sortedKeys(kinds) {
		b, err := h.ReadFile(ctx, path.Join(dir, "generated", name), false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// A missing value would make every Service using it look changed.
			return nil, fmt.Errorf("read generated secret %s: %w", name, err)
		}
		if len(b) > 0 {
			generated[name] = string(b)
		}
	}
	next, err := serviceSecrets(d, generated, func(string, ...any) {})
	if err != nil {
		return nil, err
	}
	if len(cur.Secrets) > 0 {
		b, err := h.ReadFile(ctx, path.Join(dir, "hmac.key"), false)
		if err != nil {
			return nil, fmt.Errorf("read hmac.key: %w", err)
		}
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) < 16 {
			return nil, fmt.Errorf("invalid hmac.key")
		}
		return ChangedServicesFromAudit(cur.Secrets, secretAudit(d, next, generated, key)), nil
	}
	if cur.SecretsGeneration == "" {
		return sortedKeys(next), nil
	}
	prev, err := readRemoteSecretsGeneration(ctx, h, release.SecretsDir(d.App, d.Destination, cur.SecretsGeneration))
	if err != nil {
		return nil, err
	}
	return ChangedServices(prev, next), nil
}

func readRemoteSecretsGeneration(ctx context.Context, h remote.Host, dir string) (map[string]map[string]string, error) {
	// A missing generation would make every Service look changed. macOS ls
	// colors names under CLICOLOR_FORCE (see prune).
	listing, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ ! -d " + remote.Quote(dir) + " ]; then echo " + remote.Quote("secrets generation "+path.Base(dir)+" is gone; run yoho deploy") + " >&2; exit 1; fi\nenv -u CLICOLOR_FORCE -u CLICOLOR ls -1 " + remote.Quote(dir)})
	if err != nil {
		return nil, fmt.Errorf("list secrets generation: %w", err)
	}
	out := map[string]map[string]string{}
	for _, name := range lines(listing) {
		p := path.Join(dir, name)
		if strings.HasSuffix(name, ".env") {
			b, err := h.ReadFile(ctx, p, false)
			if err != nil {
				return nil, err
			}
			m, err := parseEnvFile(b)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", name, err)
			}
			out[strings.TrimSuffix(name, ".env")] = m
			continue
		}
		files, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ -d " + remote.Quote(p) + " ]; then env -u CLICOLOR_FORCE -u CLICOLOR ls -1 " + remote.Quote(p) + "; fi"})
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		for _, f := range lines(files) {
			if !secretNameRe.MatchString(f) {
				continue
			}
			b, err := h.ReadFile(ctx, path.Join(p, f), false)
			if err != nil {
				return nil, err
			}
			m[f] = string(b)
		}
		if len(m) > 0 {
			out[name] = m
		}
	}
	return out, nil
}

// CheckRestartBase runs under the deploy lock: a Restart deploy compared
// secrets against d.RestartFrom, so that must still be the current Release.
func CheckRestartBase(ctx context.Context, h remote.Host, d *plan.Deploy) error {
	if len(d.Restart) == 0 || d.RestartFrom == nil {
		return nil
	}
	dir := release.AppDir(d.App, d.Destination)
	link, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil {
		return err
	}
	cur, err := readRelease(ctx, h, path.Join(dir, "releases", path.Base(strings.TrimSpace(link)), "release.json"))
	if err != nil || cur.Version != d.RestartFrom.Version || cur.SecretsGeneration != d.RestartFrom.SecretsGeneration {
		return fmt.Errorf("the current Release of %s/%s changed since secrets were compared; run yoho secrets rotate again", d.App, d.Destination)
	}
	return nil
}

// restartSelected reports whether name should be started on this run.
// An empty Restart list means every Service (a normal deploy).
func restartSelected(names []string, name string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
