package swarm

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// rollbackSecrets is what a swarm rollback runs: the stack file, the Swarm
// secrets it references and the generation of its env files.
type rollbackSecrets struct {
	yaml    []byte
	gen     string
	from    string
	mode    string
	write   bool
	base    []byte
	baseGen string
	audit   map[string]release.SecretAudit
	// baseAudit is the audit of the Release as first deployed.
	baseAudit map[string]release.SecretAudit
	// secrets are the Swarm secrets the stack file references; plan the
	// plan.json list (it also keeps the base file's, see prune).
	secrets []string
	plan    []string
	// create are generated values whose content-addressed secret may not
	// exist yet (name -> value).
	create map[string]string
	envs   map[string]map[string]string
	values []string
}

// fileSecret is one Swarm secret a stack Service mounts.
type fileSecret struct{ svc, target, source string }

// stackFileSecrets lists the Yoho-managed Swarm secrets of each Service.
func stackFileSecrets(doc *yaml.Node, managed map[string]bool) []fileSecret {
	var out []fileSecret
	svcs := deploy.MapGet(doc, "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(svcs.Content); i += 2 {
		name := svcs.Content[i].Value
		list := deploy.MapGet(svcs.Content[i+1], "secrets")
		if list == nil || list.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range list.Content {
			src, tgt := deploy.MapGet(item, "source"), deploy.MapGet(item, "target")
			if src == nil || tgt == nil || !managed[src.Value] {
				continue
			}
			out = append(out, fileSecret{svc: name, target: tgt.Value, source: src.Value})
		}
	}
	return out
}

// repointSecrets makes each listed Service mount repl[svc/target] and
// rewrites the top-level secrets: managed ones nothing references are
// dropped, new ones added as external.
func repointSecrets(doc *yaml.Node, managed map[string]bool, repl map[string]string) []string {
	used := map[string]bool{}
	svcs := deploy.MapGet(doc, "services")
	for i := 0; svcs != nil && i+1 < len(svcs.Content); i += 2 {
		name := svcs.Content[i].Value
		list := deploy.MapGet(svcs.Content[i+1], "secrets")
		if list == nil || list.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range list.Content {
			src, tgt := deploy.MapGet(item, "source"), deploy.MapGet(item, "target")
			if src == nil || tgt == nil || !managed[src.Value] {
				continue
			}
			if n, ok := repl[name+"/"+tgt.Value]; ok {
				src.Value = n
			}
			used[src.Value] = true
		}
	}
	top := deploy.MapGet(doc, "secrets")
	if top == nil {
		root := doc.Content[0]
		top = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "secrets"}, top)
	}
	var kept []*yaml.Node
	have := map[string]bool{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		k := top.Content[i].Value
		if managed[k] && !used[k] {
			continue
		}
		have[k] = true
		kept = append(kept, top.Content[i], top.Content[i+1])
	}
	names := sortedKeys(used)
	for _, n := range names {
		if have[n] {
			continue
		}
		kept = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Value: n}, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "external"}, {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
			{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: n},
		}})
	}
	top.Content = kept
	return names
}

// externalStackSecrets are the top-level external secrets of doc named
// <stack>_...
func externalStackSecrets(doc *yaml.Node, stack string) map[string]bool {
	out := map[string]bool{}
	top := deploy.MapGet(doc, "secrets")
	for i := 0; top != nil && i+1 < len(top.Content); i += 2 {
		k, v := top.Content[i].Value, top.Content[i+1]
		if ext := deploy.MapGet(v, "external"); ext != nil && ext.Value == "true" && strings.HasPrefix(k, stack+"_") {
			out[k] = true
		}
	}
	return out
}

// stackSecretNames lists the managed Swarm secrets doc references.
func stackSecretNames(doc *yaml.Node, managed map[string]bool) []string {
	set := map[string]bool{}
	for _, f := range stackFileSecrets(doc, managed) {
		set[f.source] = true
	}
	return sortedKeys(set)
}

// planRollbackSecrets is the swarm counterpart of the compose runtime's:
// the target Release's stack file, re-pointed at the current Release's
// Swarm secrets and env values (generated values from generated/).
func (r *runner) planRollbackSecrets(ctx context.Context, d *plan.Deploy, rel *release.Release, p *stackPlan, onDisk, base []byte, baseGen, newGen string) (*rollbackSecrets, error) {
	h := r.h
	managed := map[string]bool{}
	for _, s := range p.Secrets {
		managed[s] = true
	}
	doc, err := deploy.ParseYAMLDoc(base)
	if err != nil {
		return nil, fmt.Errorf("release %s: stack file: %w", rel.Version, err)
	}
	baseNames := stackSecretNames(doc, managed)
	ba := deploy.BaseAudit(rel)
	if d.RollbackReleaseSecrets {
		return &rollbackSecrets{yaml: base, gen: baseGen, from: baseGen, mode: deploy.RollbackSecretsRelease, write: !bytes.Equal(base, onDisk),
			audit: ba, secrets: baseNames, plan: baseNames}, nil
	}
	cur, err := deploy.CurrentRelease(ctx, h, d.App, d.Destination)
	if err != nil {
		return nil, err
	}
	keep := &rollbackSecrets{yaml: onDisk, gen: rel.SecretsGeneration, mode: deploy.RollbackSecretsCurrent, audit: rel.Secrets, baseAudit: ba, plan: p.Secrets}
	if rel.BaseComposeSHA256 != "" {
		keep.base, keep.baseGen = base, baseGen
	}
	if od, err := deploy.ParseYAMLDoc(onDisk); err == nil {
		keep.secrets = stackSecretNames(od, managed)
	}
	if cur != nil {
		keep.from = cur.SecretsGeneration
	}
	baseDir := release.SecretsDir(d.App, d.Destination, baseGen)
	files := stackFileSecrets(doc, managed)
	_, envs := deploy.GenSlots(doc, baseDir)
	if len(files)+len(envs) == 0 {
		return keep, nil
	}
	if cur != nil && (cur.SecretsGeneration == rel.SecretsGeneration || cur.SecretsGeneration == baseGen) {
		if cur.SecretsGeneration == baseGen {
			keep.yaml, keep.gen, keep.write, keep.base, keep.baseGen, keep.audit = base, baseGen, !bytes.Equal(base, onDisk), nil, "", ba
			keep.secrets, keep.plan = baseNames, baseNames
		}
		return keep, nil
	}

	// What the current Release delivers: Swarm secret names per Service
	// and target, env values per Service.
	curNames := map[string]map[string]string{}
	curEnv := map[string]map[string]string{}
	var curAudit map[string]release.SecretAudit
	var curGen string
	if cur != nil {
		curGen, curAudit = cur.SecretsGeneration, cur.Secrets
		curDir := release.Dir(d.App, d.Destination, cur.Version)
		cy, err := h.ReadFile(ctx, path.Join(curDir, "compose.yaml"), false)
		if err != nil {
			return nil, fmt.Errorf("current Release %s has no stack file: %w", cur.Version, err)
		}
		cdoc, err := deploy.ParseYAMLDoc(cy)
		if err != nil {
			return nil, fmt.Errorf("current Release %s: stack file: %w", cur.Version, err)
		}
		cm := map[string]bool{}
		cp, err := readPlan(ctx, h, curDir)
		if err == nil {
			for _, s := range cp.Secrets {
				cm[s] = true
			}
		} else {
			// No plan.json (older or damaged record): Yoho's secrets are
			// the external ones named after the stack.
			cp = &stackPlan{EnvFiles: true}
			cm = externalStackSecrets(cdoc, r.stack)
		}
		for _, f := range stackFileSecrets(cdoc, cm) {
			if curNames[f.svc] == nil {
				curNames[f.svc] = map[string]string{}
			}
			curNames[f.svc][f.target] = f.source
		}
		if len(envs) > 0 && curGen != "" && cp.EnvFiles {
			if curEnv, err = deploy.ReadRemoteSecretsGeneration(ctx, h, d.App, d.Destination, curGen); err != nil {
				return nil, fmt.Errorf("current Release %s: %w", cur.Version, err)
			}
		}
	}
	key, err := deploy.EnsureHMACKey(ctx, h, r.dir)
	if err != nil {
		return nil, err
	}
	newGen = deploy.FreshGeneration(newGen, baseGen, rel.SecretsGeneration, curGen)
	out := &rollbackSecrets{gen: newGen, from: curGen, mode: deploy.RollbackSecretsCurrent, write: true, base: base, baseGen: baseGen, baseAudit: ba,
		audit: map[string]release.SecretAudit{}, create: map[string]string{}, envs: map[string]map[string]string{}}
	var missing []string
	generated := func(svc, name string) (string, bool, error) {
		if ba[svc+"/"+name].Ref != "generated" {
			return "", false, nil
		}
		return deploy.ReadGenerated(ctx, h, d.App, d.Destination, name)
	}
	repl := map[string]string{}
	for _, f := range files {
		k := f.svc + "/" + f.target
		v, ok, err := generated(f.svc, f.target)
		if err != nil {
			return nil, err
		}
		if ok {
			sn := SecretName(r.stack, f.target, v, key)
			repl[k], out.create[sn] = sn, v
			out.audit[k] = release.SecretAudit{Ref: "generated", Fingerprint: secrets.Fingerprint(key, v)}
			out.values = append(out.values, v)
			continue
		}
		sn, from, ok := deploy.PickCurrent(f.svc, f.target, ba, curAudit, curNames)
		if !ok {
			missing = append(missing, k)
			continue
		}
		repl[k] = sn
		out.audit[k] = curAudit[from]
	}
	var baseVals map[string]map[string]string
	for _, svc := range envs {
		names := deploy.EnvSecretNames(ba, svc, nil)
		if len(names) == 0 {
			if baseVals == nil {
				if baseVals, err = deploy.ReadRemoteSecretsGeneration(ctx, h, d.App, d.Destination, baseGen); err != nil {
					return nil, fmt.Errorf("release %s: cannot tell which secrets %s reads: %w", rel.Version, svc, err)
				}
			}
			names = deploy.EnvSecretNames(nil, svc, baseVals)
		}
		m := map[string]string{}
		for _, name := range names {
			k := svc + "/" + name
			v, ok, err := generated(svc, name)
			if err != nil {
				return nil, err
			}
			ref := "generated"
			if !ok {
				var from string
				v, from, ok = deploy.PickCurrent(svc, name, ba, curAudit, curEnv)
				ref = curAudit[from].Ref
			}
			if !ok {
				missing = append(missing, k)
				continue
			}
			m[name] = v
			out.audit[k] = release.SecretAudit{Ref: ref, Fingerprint: secrets.Fingerprint(key, v)}
			out.values = append(out.values, v)
		}
		out.envs[svc] = m
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		curVersion := ""
		if cur != nil {
			curVersion = cur.Version
		}
		return nil, &deploy.MissingSecretsError{Version: rel.Version, Current: curVersion, Missing: missing}
	}
	newDir := release.SecretsDir(d.App, d.Destination, newGen)
	out.secrets = repointSecrets(doc, managed, repl)
	deploy.RepointGeneration(doc, baseDir, newDir)
	for svc, m := range out.envs {
		deploy.SetSecretsLabel(doc, svc, secretSetFingerprint(key, m))
	}
	if out.yaml, err = deploy.EncodeYAMLDoc(doc); err != nil {
		return nil, err
	}
	planSet := map[string]bool{}
	for _, s := range append(slices.Clone(out.secrets), baseNames...) {
		planSet[s] = true
	}
	out.plan = sortedKeys(planSet)
	return out, nil
}

// apply writes the env files and creates the generated Swarm secrets of a
// current-secrets rollback.
func (s *rollbackSecrets) apply(ctx context.Context, r *runner, d *plan.Deploy) error {
	if len(s.envs) > 0 {
		dir := release.SecretsDir(d.App, d.Destination, s.gen)
		if err := r.h.Run(ctx, remote.Cmd{Script: "set -eu\numask 077\nmkdir -p " + remote.Quote(dir)}); err != nil {
			return fmt.Errorf("create secrets generation: %w", err)
		}
		for _, svc := range sortedKeys(s.envs) {
			if err := r.h.WriteFile(ctx, path.Join(dir, svc+".env"), deploy.EnvFile(s.envs[svc]), 0o600, false); err != nil {
				return fmt.Errorf("write secrets for %s: %w", svc, err)
			}
		}
	}
	for _, sn := range sortedKeys(s.create) {
		if err := r.createSecret(ctx, d, sn, s.create[sn]); err != nil {
			return err
		}
	}
	return nil
}

// record sets what the rollback ran on the Release record.
func (s *rollbackSecrets) record(rel *release.Release) {
	rel.SecretsGeneration = s.gen
	rel.ComposeSHA256 = deploy.SHA256Hex(s.yaml)
	rel.RollbackSecrets, rel.SecretsFrom = s.mode, s.from
	rel.Secrets = s.audit
	rel.BaseComposeSHA256, rel.BaseSecretsGeneration, rel.BaseSecrets = "", "", nil
	if s.base != nil {
		rel.BaseComposeSHA256, rel.BaseSecretsGeneration, rel.BaseSecrets = deploy.SHA256Hex(s.base), s.baseGen, s.baseAudit
	}
}
