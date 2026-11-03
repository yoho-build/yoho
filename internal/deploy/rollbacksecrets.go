package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Release.RollbackSecrets values.
const (
	RollbackSecretsCurrent = "current"
	RollbackSecretsRelease = "release"
)

// BaseComposeFile is the compiled file of a Release as first deployed. A
// rollback with the current secrets writes it before re-pointing
// compose.yaml at another secrets generation (Release.BaseComposeSHA256).
const BaseComposeFile = "compose.release.yaml"

// MissingSecretsError refuses a rollback with the current secrets: the
// target Release reads secrets the current Release does not deliver.
type MissingSecretsError struct {
	Version string
	Current string
	// Missing are "<Service>/<NAME>", sorted.
	Missing []string
}

func (e *MissingSecretsError) Error() string {
	cur := e.Current
	if cur == "" {
		cur = "(none)"
	}
	return fmt.Sprintf("Release %s reads secrets the current Release %s does not have: %s (a rollback keeps the current secret values)",
		e.Version, cur, strings.Join(e.Missing, ", "))
}

// MissingSecretsHint is the next step for a MissingSecretsError.
const MissingSecretsHint = "deploy a Version that declares these secrets (their values must be in .yoho/secrets), or roll back with --with-release-secrets to run that Release's own values; secrets rotated since then come back"

// CurrentRelease reads the Release `current` points at; nil when there is
// none.
func CurrentRelease(ctx context.Context, h remote.Host, app, dest string) (*release.Release, error) {
	dir := release.AppDir(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil {
		return nil, fmt.Errorf("read current Release: %w", err)
	}
	v := path.Base(strings.TrimSpace(out))
	if v == "" || v == "." || v == "/" {
		return nil, nil
	}
	rel, err := readRelease(ctx, h, path.Join(release.Dir(app, dest, v), "release.json"))
	if err != nil {
		return nil, fmt.Errorf("current Release %s: %w", v, err)
	}
	return rel, nil
}

// BaseCompose returns the compiled file and secrets generation of rel as
// first deployed: compose.release.yaml when a rollback re-pointed
// compose.yaml (onDisk) at other secrets, else onDisk itself.
func BaseCompose(ctx context.Context, h remote.Host, app, dest string, rel *release.Release, onDisk []byte) ([]byte, string, error) {
	if rel.BaseComposeSHA256 == "" {
		return onDisk, rel.SecretsGeneration, nil
	}
	b, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, rel.Version), BaseComposeFile), false)
	if err != nil {
		return nil, "", fmt.Errorf("release %s has no %s: %w", rel.Version, BaseComposeFile, err)
	}
	if sha256Hex(b) != rel.BaseComposeSHA256 {
		return nil, "", fmt.Errorf("release %s: %s does not match its recorded sha256", rel.Version, BaseComposeFile)
	}
	return b, rel.BaseSecretsGeneration, nil
}

// BaseAudit is the secret audit of rel as first deployed.
func BaseAudit(rel *release.Release) map[string]release.SecretAudit {
	if rel.BaseComposeSHA256 != "" && rel.BaseSecrets != nil {
		return rel.BaseSecrets
	}
	return rel.Secrets
}

// VetCopy is rel as the Gate recorded it: the base compiled file's hash.
func VetCopy(rel *release.Release) *release.Release {
	c := *rel
	if c.BaseComposeSHA256 != "" {
		c.ComposeSHA256 = c.BaseComposeSHA256
	}
	return &c
}

// ReadGenerated reads generated/<name>; ok is false when it does not exist
// or is empty.
func ReadGenerated(ctx context.Context, h remote.Host, app, dest, name string) (string, bool, error) {
	if !secretNameRe.MatchString(name) {
		return "", false, nil
	}
	b, err := h.ReadFile(ctx, path.Join(release.AppDir(app, dest), "generated", name), false)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read generated secret %s: %w", name, err)
	}
	return string(b), len(b) > 0, nil
}

// ReadRemoteSecretsGeneration reads a generation on h into Service ->
// container name -> value. An empty generation name reads as empty.
func ReadRemoteSecretsGeneration(ctx context.Context, h remote.Host, app, dest, generation string) (map[string]map[string]string, error) {
	if generation == "" {
		return map[string]map[string]string{}, nil
	}
	return readRemoteSecretsGeneration(ctx, h, release.SecretsDir(app, dest, generation))
}

// PickCurrent finds the current counterpart of the target Release's
// secret svc/name in vals (the current Release's, by Service and name):
// the same Service's, else the one other Services read from the same
// provider reference, when they agree. from is its "<Service>/<NAME>".
func PickCurrent[T comparable](svc, name string, target, cur map[string]release.SecretAudit, vals map[string]map[string]T) (v T, from string, ok bool) {
	if v, ok := vals[svc][name]; ok {
		return v, svc + "/" + name, true
	}
	want := target[svc+"/"+name].Ref
	if want == "" || want == "generated" {
		return v, "", false
	}
	for _, s := range sortedKeys(vals) {
		got, has := vals[s][name]
		if !has || cur[s+"/"+name].Ref != want {
			continue
		}
		if from != "" && got != v {
			var zero T
			return zero, "", false
		}
		v, from = got, s+"/"+name
	}
	return v, from, from != ""
}

// FreshGeneration is gen, or gen with a "-rollback" suffix when it names
// one of taken (a rollback within the second of a deploy of the same
// Version): a generation is never rewritten.
func FreshGeneration(gen string, taken ...string) string {
	for slices.Contains(taken, gen) {
		gen += "-rollback"
	}
	return gen
}

// GenSlots is genSlots for the swarm runtime.
func GenSlots(doc *yaml.Node, genDir string) (files [][2]string, envs []string) {
	return genSlots(doc, genDir)
}

// genSlots lists what a compiled file reads from the secrets generation
// genDir: secret files "<Service>/<NAME>" and env files (Services).
func genSlots(doc *yaml.Node, genDir string) (files [][2]string, envs []string) {
	prefix := genDir + "/"
	walkScalars(doc, func(n *yaml.Node) {
		rest, ok := strings.CutPrefix(n.Value, prefix)
		if !ok {
			return
		}
		if svc, ok := strings.CutSuffix(rest, ".env"); ok && !strings.Contains(svc, "/") {
			if !slices.Contains(envs, svc) {
				envs = append(envs, svc)
			}
			return
		}
		if svc, name, ok := strings.Cut(rest, "/"); ok && !strings.Contains(name, "/") {
			f := [2]string{svc, name}
			if !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	})
	slices.Sort(envs)
	slices.SortFunc(files, func(a, b [2]string) int { return strings.Compare(a[0]+"/"+a[1], b[0]+"/"+b[1]) })
	return files, envs
}

// EnvSecretNames lists the secret names of svc's env file in the target
// Release: from its audit, else from its generation (when still there).
func EnvSecretNames(target map[string]release.SecretAudit, svc string, gen map[string]map[string]string) []string {
	var names []string
	for k := range target {
		if s, n, ok := strings.Cut(k, "/"); ok && s == svc {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		names = sortedKeys(gen[svc])
	}
	slices.Sort(names)
	return names
}

// ParseYAMLDoc parses a compiled compose or stack file for editing.
func ParseYAMLDoc(b []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	return &doc, nil
}

// EncodeYAMLDoc encodes an edited document (scalars keep their text, so an
// escaped `$$` stays escaped).
func EncodeYAMLDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MapGet returns the value node of key in mapping n, or nil.
func MapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// SetSecretsLabel sets services.<svc>.labels.<LabelSecrets> to fp where
// the label already exists (a mapping of labels).
func SetSecretsLabel(doc *yaml.Node, svc, fp string) {
	if l := MapGet(MapGet(MapGet(doc, "services"), svc), "labels"); l != nil {
		if v := MapGet(l, LabelSecrets); v != nil && v.Kind == yaml.ScalarNode {
			v.Value = fp
		}
	}
}

// RepointGeneration replaces the generation directory from with to in
// every path of doc.
func RepointGeneration(doc *yaml.Node, from, to string) {
	walkScalars(doc, func(n *yaml.Node) {
		if rest, ok := strings.CutPrefix(n.Value, from+"/"); ok {
			n.Value = to + "/" + rest
		}
	})
}

func walkScalars(n *yaml.Node, f func(*yaml.Node)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		f(n)
		return
	}
	for _, c := range n.Content {
		walkScalars(c, f)
	}
}

// SecretSetFingerprint is the yoho.secrets label value for a Service's
// secret set.
func SecretSetFingerprint(key []byte, m map[string]string) string { return secretsFingerprint(key, m) }

// currentSecrets is a compose rollback's plan for its secrets.
type currentSecrets struct {
	yaml    []byte // compiled file to run
	gen     string // secrets generation it reads
	from    string // generation the values came from
	mode    string // RollbackSecretsCurrent | RollbackSecretsRelease
	write   bool   // yaml differs from compose.yaml on disk
	base    []byte // when set: write as BaseComposeFile
	baseGen string
	audit   map[string]release.SecretAudit
	// baseAudit is the audit of the Release as first deployed.
	baseAudit map[string]release.SecretAudit
	values    []string // for redaction
}

// planRollbackSecrets decides which compiled file and secrets generation a
// compose rollback to rel runs, and writes the new generation when the
// current secrets differ from the ones the Release reads (see
// RollbackSecretsCurrent). onDisk is releases/<version>/compose.yaml.
func planRollbackSecrets(ctx context.Context, h remote.Host, app, dest string, rel *release.Release, onDisk []byte, base []byte, baseGen string, releaseSecrets bool, newGen string) (*currentSecrets, error) {
	ba := BaseAudit(rel)
	if releaseSecrets {
		return &currentSecrets{yaml: base, gen: baseGen, from: baseGen, mode: RollbackSecretsRelease, write: !bytes.Equal(base, onDisk), audit: ba}, nil
	}
	cur, err := CurrentRelease(ctx, h, app, dest)
	if err != nil {
		return nil, err
	}
	keep := &currentSecrets{yaml: onDisk, gen: rel.SecretsGeneration, mode: RollbackSecretsCurrent, audit: rel.Secrets, baseAudit: ba}
	if rel.BaseComposeSHA256 != "" {
		keep.base, keep.baseGen = base, baseGen
	}
	doc, err := ParseYAMLDoc(base)
	if err != nil {
		return nil, fmt.Errorf("release %s: compose.yaml: %w", rel.Version, err)
	}
	baseDir := release.SecretsDir(app, dest, baseGen)
	files, envs := genSlots(doc, baseDir)
	if baseGen == "" || len(files)+len(envs) == 0 {
		// Reads no secrets: nothing to keep current.
		keep.from = ""
		if cur != nil {
			keep.from = cur.SecretsGeneration
		}
		return keep, nil
	}
	if cur != nil && (cur.SecretsGeneration == rel.SecretsGeneration || cur.SecretsGeneration == baseGen) {
		// The current secrets are the ones the Release reads already.
		keep.from = cur.SecretsGeneration
		if cur.SecretsGeneration == baseGen {
			keep.yaml, keep.gen, keep.write, keep.base, keep.baseGen, keep.audit = base, baseGen, !bytes.Equal(base, onDisk), nil, "", ba
		}
		return keep, nil
	}
	var curGen string
	var curAudit map[string]release.SecretAudit
	vals := map[string]map[string]string{}
	if cur != nil {
		curGen, curAudit = cur.SecretsGeneration, cur.Secrets
		if vals, err = ReadRemoteSecretsGeneration(ctx, h, app, dest, curGen); err != nil {
			return nil, fmt.Errorf("current Release %s: %w", cur.Version, err)
		}
	}
	var baseVals map[string]map[string]string
	needed := map[string][]string{}
	asEnv := map[string]bool{}
	for _, f := range files {
		needed[f[0]] = append(needed[f[0]], f[1])
	}
	for _, svc := range envs {
		asEnv[svc] = true
		names := EnvSecretNames(ba, svc, nil)
		if len(names) == 0 {
			if baseVals == nil {
				if baseVals, err = ReadRemoteSecretsGeneration(ctx, h, app, dest, baseGen); err != nil {
					return nil, fmt.Errorf("release %s: cannot tell which secrets %s reads: %w", rel.Version, svc, err)
				}
			}
			names = EnvSecretNames(nil, svc, baseVals)
		}
		needed[svc] = append(needed[svc], names...)
	}
	key, err := ensureHMACKey(ctx, h, release.AppDir(app, dest))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	audit := map[string]release.SecretAudit{}
	var missing, values []string
	for _, svc := range sortedKeys(needed) {
		for _, name := range needed[svc] {
			v, ref, ok := "", "", false
			if ba[svc+"/"+name].Ref == "generated" {
				if v, ok, err = ReadGenerated(ctx, h, app, dest, name); err != nil {
					return nil, err
				}
				ref = "generated"
			}
			if !ok {
				var from string
				v, from, ok = PickCurrent(svc, name, ba, curAudit, vals)
				ref = curAudit[from].Ref
			}
			if !ok {
				missing = append(missing, svc+"/"+name)
				continue
			}
			if out[svc] == nil {
				out[svc] = map[string]string{}
			}
			out[svc][name] = v
			audit[svc+"/"+name] = release.SecretAudit{Ref: ref, Fingerprint: secrets.Fingerprint(key, v)}
			values = append(values, v)
		}
	}
	if len(missing) > 0 {
		curVersion := ""
		if cur != nil {
			curVersion = cur.Version
		}
		return nil, &MissingSecretsError{Version: rel.Version, Current: curVersion, Missing: missing}
	}
	newGen = FreshGeneration(newGen, baseGen, rel.SecretsGeneration, curGen)
	newDir := release.SecretsDir(app, dest, newGen)
	if err := writeSecrets(ctx, h, newDir, out, asEnv); err != nil {
		return nil, err
	}
	RepointGeneration(doc, baseDir, newDir)
	for svc, m := range out {
		SetSecretsLabel(doc, svc, secretsFingerprint(key, m))
	}
	eff, err := EncodeYAMLDoc(doc)
	if err != nil {
		return nil, err
	}
	return &currentSecrets{yaml: eff, gen: newGen, from: curGen, mode: RollbackSecretsCurrent, write: true,
		base: base, baseGen: baseGen, audit: audit, baseAudit: ba, values: values}, nil
}

// record sets what the rollback ran on the Release record.
func (s *currentSecrets) record(rel *release.Release) {
	rel.SecretsGeneration = s.gen
	rel.ComposeSHA256 = sha256Hex(s.yaml)
	rel.RollbackSecrets, rel.SecretsFrom = s.mode, s.from
	rel.Secrets = s.audit
	rel.BaseComposeSHA256, rel.BaseSecretsGeneration, rel.BaseSecrets = "", "", nil
	if s.base != nil {
		rel.BaseComposeSHA256, rel.BaseSecretsGeneration, rel.BaseSecrets = sha256Hex(s.base), s.baseGen, s.baseAudit
	}
}

// describe is the progress line naming whose secrets the rollback runs.
func (s *currentSecrets) describe(version string) string {
	from := s.from
	if from == "" {
		from = "none"
	}
	if s.mode == RollbackSecretsRelease {
		return "secrets: Release " + version + "'s own values (generation " + from + ")"
	}
	return "secrets: the current values (from generation " + from + ")"
}

// writeRollbackFiles writes the compiled file a rollback runs (and the
// Release's original next to it).
func writeRollbackFiles(ctx context.Context, h remote.Host, relDir string, s *currentSecrets) error {
	if s.base != nil {
		if err := h.WriteFile(ctx, path.Join(relDir, BaseComposeFile), s.base, 0o600, false); err != nil {
			return fmt.Errorf("write %s: %w", BaseComposeFile, err)
		}
	}
	if err := h.WriteFile(ctx, path.Join(relDir, "compose.yaml"), s.yaml, 0o600, false); err != nil {
		return fmt.Errorf("write compose.yaml: %w", err)
	}
	if err := h.WriteFile(ctx, path.Join(relDir, "compose_sha256"), []byte(sha256Hex(s.yaml)+"\n"), 0o600, false); err != nil {
		return fmt.Errorf("write compose_sha256: %w", err)
	}
	return nil
}
