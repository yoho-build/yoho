package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Secret names become file names and env var names (<NAME>_FILE).
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ensureFileOnce creates path with data (0600) unless it already exists, and
// returns the stored content. Never overwrites: `ln` fails atomically when
// the target exists. data travels over stdin.
func ensureFileOnce(ctx context.Context, h remote.Host, p string, data []byte) ([]byte, error) {
	q := remote.Quote(p)
	script := "set -eu\numask 077\nmkdir -p " + remote.Quote(path.Dir(p)) + "\n" +
		"if [ ! -e " + q + " ]; then\n" +
		"  t=$(mktemp " + remote.Quote(p+".XXXXXX") + ")\n" +
		"  cat > \"$t\"\n  chmod 600 \"$t\"\n" +
		"  ln \"$t\" " + q + " 2>/dev/null || true\n  rm -f \"$t\"\nfi"
	if err := h.Run(ctx, remote.Cmd{Script: script, Stdin: bytes.NewReader(data)}); err != nil {
		return nil, fmt.Errorf("create %s: %w", p, err)
	}
	return h.ReadFile(ctx, p, false)
}

// ensureHMACKey returns the per-Destination audit key, creating it once
// (32 random bytes, stored hex-encoded so it survives text transports).
func ensureHMACKey(ctx context.Context, h remote.Host, dir string) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	stored, err := ensureFileOnce(ctx, h, path.Join(dir, "hmac.key"), []byte(hex.EncodeToString(b)))
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(stored)))
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("invalid hmac.key in %s", dir)
	}
	return key, nil
}

// generatedDecls collects x-yoho.generate across Services: key -> kind.
func generatedDecls(d *plan.Deploy) (map[string]string, error) {
	kinds := map[string]string{}
	owner := map[string]string{}
	for _, svc := range sortedKeys(d.Ext) {
		for name, kind := range d.Ext[svc].Generate {
			if !secretNameRe.MatchString(name) {
				return nil, fmt.Errorf("service %s: invalid generated secret name %q", svc, name)
			}
			if prev, ok := kinds[name]; ok && prev != kind {
				return nil, fmt.Errorf("generated secret %s declared as %s by %s and %s by %s", name, prev, owner[name], kind, svc)
			}
			kinds[name] = kind
			owner[name] = svc
		}
	}
	return kinds, nil
}

// ensureGenerated creates missing generated secrets under dir/generated and
// returns all their values. Existing values are never replaced.
func ensureGenerated(ctx context.Context, h remote.Host, dir string, kinds map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range sortedKeys(kinds) {
		v, err := secrets.Generate(kinds[name])
		if err != nil {
			return nil, fmt.Errorf("generated secret %s: %w", name, err)
		}
		stored, err := ensureFileOnce(ctx, h, path.Join(dir, "generated", name), []byte(v))
		if err != nil {
			return nil, err
		}
		if len(stored) == 0 {
			return nil, fmt.Errorf("generated secret %s is empty on the Server", name)
		}
		out[name] = string(stored)
	}
	return out, nil
}

// serviceSecrets builds each Service's container-name -> value map: values
// the CLI resolved locally, plus refs to generated keys, plus generated
// secrets the Service declares itself (under their own name).
func serviceSecrets(d *plan.Deploy, generated map[string]string, warn func(string, ...any)) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, svc := range sortedKeys(d.Project.Services) {
		m := map[string]string{}
		for k, v := range d.ServiceSecrets[svc] {
			m[k] = v
		}
		ext := d.Ext[svc]
		for _, ref := range ext.Secrets {
			if _, ok := m[ref.Name]; ok {
				continue
			}
			v, ok := generated[ref.Key]
			if !ok {
				return nil, fmt.Errorf("service %s: secret %s (key %s) was not resolved", svc, ref.Name, ref.Key)
			}
			m[ref.Name] = v
		}
		for _, name := range sortedKeys(ext.Generate) {
			if _, ok := m[name]; ok {
				if m[name] != generated[name] {
					warn("service %s: secret %s is set locally and also generated; using the local value", svc, name)
				}
				continue
			}
			m[name] = generated[name]
		}
		for name := range m {
			if !secretNameRe.MatchString(name) {
				return nil, fmt.Errorf("service %s: invalid secret name %q (letters, digits, underscore)", svc, name)
			}
		}
		if len(m) > 0 {
			out[svc] = m
		}
	}
	return out, nil
}

// secretAudit returns the Release audit records: "<service>/<NAME>" ->
// provider ref + keyed fingerprint. Never the value.
func secretAudit(d *plan.Deploy, svcSecrets map[string]map[string]string, generated map[string]string, key []byte) map[string]release.SecretAudit {
	out := map[string]release.SecretAudit{}
	for svc, m := range svcSecrets {
		keyOf := map[string]string{}
		for _, ref := range d.Ext[svc].Secrets {
			keyOf[ref.Name] = ref.Key
		}
		for name, v := range m {
			k := keyOf[name]
			if k == "" {
				k = name
			}
			ref := d.SecretRefs[k]
			if g, ok := generated[k]; ref == "" && ok && g == v {
				ref = "generated"
			}
			out[svc+"/"+name] = release.SecretAudit{Ref: ref, Fingerprint: secrets.Fingerprint(key, v)}
		}
	}
	return out
}

// secretsFingerprint is a keyed digest of a Service's whole secret set, used
// as a label so compose recreates the Service when a value changes (file
// paths live in top-level secrets, which compose does not hash).
func secretsFingerprint(key []byte, m map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(m[k])
		b.WriteByte(0)
	}
	return secrets.Fingerprint(key, b.String())
}

// writeSecrets writes a generation: <gen>/<service>/<NAME> (0444, dirs 0700)
// or <gen>/<service>.env (0600) for Services using env delivery.
func writeSecrets(ctx context.Context, h remote.Host, genDir string, svcSecrets map[string]map[string]string, asEnv map[string]bool) error {
	dirs := []string{genDir}
	for _, svc := range sortedKeys(svcSecrets) {
		if !asEnv[svc] {
			dirs = append(dirs, path.Join(genDir, svc))
		}
	}
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\numask 077\nmkdir -p " + remote.QuoteArgs(dirs...)}); err != nil {
		return fmt.Errorf("create secrets generation: %w", err)
	}
	for _, svc := range sortedKeys(svcSecrets) {
		m := svcSecrets[svc]
		if asEnv[svc] {
			if err := h.WriteFile(ctx, path.Join(genDir, svc+".env"), envFile(m), 0o600, false); err != nil {
				return fmt.Errorf("write secrets for %s: %w", svc, err)
			}
			continue
		}
		for _, name := range sortedKeys(m) {
			if err := h.WriteFile(ctx, path.Join(genDir, svc, name), []byte(m[name]), 0o444, false); err != nil {
				return fmt.Errorf("write secret %s for %s: %w", name, svc, err)
			}
		}
	}
	return nil
}

// envFile renders a compose env_file. Values are double-quoted with
// backslash, quote, dollar and newline escaped, which the compose dotenv
// parser reverses exactly (no interpolation).
func envFile(m map[string]string) []byte {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`)
	var b bytes.Buffer
	for _, k := range sortedKeys(m) {
		b.WriteString(k + "=\"" + r.Replace(m[k]) + "\"\n")
	}
	return b.Bytes()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
