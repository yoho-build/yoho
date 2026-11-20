package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/version"
)

// savedPlan is the stable v1 approval artifact. Digests contain no raw inputs.
type savedPlan struct {
	Format         int                      `json:"format_version"`
	App            string                   `json:"app"`
	Destination    string                   `json:"destination"`
	Servers        map[string]config.Server `json:"servers"`
	Runtime        string                   `json:"runtime"`
	Yoho           string                   `json:"yoho_version"`
	Config         string                   `json:"config_digest"`
	Secrets        string                   `json:"secret_inventory_digest"`
	Version        string                   `json:"target_version"`
	SourceRevision string                   `json:"source_revision,omitempty"`
	Inputs         string                   `json:"build_input_digest"`
	Remote         map[string]string        `json:"remote_state"`
	Changes        []plan.Change            `json:"changes"`
	Created        time.Time                `json:"created_at"`
	Expires        time.Time                `json:"expires_at"`
}

func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func stalePlan(format string, args ...any) error {
	return &exitError{15, fmt.Errorf("plan is stale or invalid: "+format, args...)}
}
func readSavedPlan(file string) (*savedPlan, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, stalePlan("%v", err)
	}
	defer f.Close()
	var p savedPlan
	dec := json.NewDecoder(io.LimitReader(f, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, stalePlan("%v", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, stalePlan("trailing data")
	}
	return &p, nil
}
func (p *savedPlan) validate(app, dest, yoho string, now time.Time) error {
	switch {
	case p.Format != 1:
		return stalePlan("unsupported format version")
	case p.App != app:
		return stalePlan("App changed")
	case p.Destination != dest:
		return stalePlan("Destination changed")
	case p.Yoho != yoho:
		return stalePlan("Yoho version changed (%s → %s)", p.Yoho, yoho)
	case p.Created.IsZero() || p.Created.After(now) || !p.Expires.After(p.Created) || !now.Before(p.Expires):
		return stalePlan("expired or invalid timestamps")
	case !version.Valid(p.Version) || p.Config == "" || p.Inputs == "" || p.Secrets == "":
		return stalePlan("missing digests or target version")
	}
	return nil
}
func (p *savedPlan) compare(q *savedPlan) error {
	a, b := *p, *q
	a.Created, b.Created = time.Time{}, time.Time{}
	a.Expires, b.Expires = time.Time{}, time.Time{}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	typ := av.Type()
	var diff []string
	for i := 0; i < av.NumField(); i++ {
		x, _ := json.Marshal(av.Field(i).Interface())
		y, _ := json.Marshal(bv.Field(i).Interface())
		if !bytes.Equal(x, y) {
			diff = append(diff, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]+" changed")
		}
	}
	if len(diff) > 0 {
		return stalePlan("%s", strings.Join(diff, "; "))
	}
	return nil
}
func (a *app) snapshotPlan(ctx context.Context, s *session, changes []plan.Change, ttl time.Duration) (*savedPlan, error) {
	p := &savedPlan{Format: 1, App: a.cfg.App, Destination: a.destName, Runtime: runtimeName(a.dest), Yoho: Version, Version: s.ver, Changes: changes, Remote: map[string]string{}, Created: time.Now().UTC()}
	p.Expires = p.Created.Add(ttl)
	if head, e := version.Git(a.dir, "rev-parse", "--verify", "HEAD"); e == nil {
		p.SourceRevision = strings.TrimSpace(string(head))
	}
	servers := map[string]config.Server{}
	for _, h := range s.hosts {
		servers[h.Name] = h.Server
	}
	p.Servers = servers
	var err error
	p.Config, err = digest([]any{a.cfg, s.r.Project, s.r.Ext})
	if err != nil {
		return nil, err
	}
	p.Inputs, err = build.InputsDigest(build.Options{Dir: a.dir, Project: s.r.Project, Builder: a.cfg.Builder})
	if err != nil {
		return nil, err
	}
	keys, err := a.secretKeys(s.r)
	if err != nil {
		return nil, err
	}
	inventory := map[string]any{"names": keys}
	for _, h := range s.hosts {
		rel, e := deploy.CurrentRelease(ctx, h.Host, a.cfg.App, a.destName)
		if e != nil {
			return nil, e
		}
		p.Remote[h.Name], e = digest(rel)
		if e != nil {
			return nil, e
		}
		if rel != nil {
			inventory[h.Name] = rel.SecretsGeneration
		}
	}
	p.Secrets, err = digest(inventory)
	return p, err
}
func (a *app) savePlan(ctx context.Context, ver, file string, ttl time.Duration) (err error) {
	defer func() { a.ui.Finished(err, "") }()
	if a.scoped {
		return stalePlan("saved plans are unavailable for Members; ask an Operator")
	}
	if ttl <= 0 {
		return stalePlan("--ttl must be positive")
	}
	ver, err = a.resolveDeployVersion(ver)
	if err != nil {
		return err
	}
	a.ui.Title("Planning %s@%s on %s", a.cfg.App, shortVersion(ver), a.destName)
	s, err := a.openSession(ctx, ver, true, false)
	if err != nil {
		return err
	}
	defer s.close()
	changes, _, err := a.collectChanges(ctx, s, false)
	if err != nil {
		return err
	}
	a.ui.Plan(changes)
	p, err := a.snapshotPlan(ctx, s, changes, ttl)
	if err != nil {
		return err
	}
	b, err := encodeSavedPlan(p, s.store.RedactString)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(file), ".yoho-plan-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), file)
}

func (a *app) reloadPlanConfig() error {
	cfg, name, overlay, err := config.LoadForDestination(a.path, a.destName)
	if err != nil {
		return stalePlan("config: %v", err)
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		return stalePlan("config is no longer valid")
	}
	if cfg.App != a.cfg.App || name != a.destName {
		return stalePlan("App or Destination changed")
	}
	dest, err := cfg.Dest(name)
	if err != nil {
		return stalePlan("%v", err)
	}
	a.cfg, a.dest, a.overlay = cfg, dest, overlay
	return nil
}

// Redact decoded strings before JSON encoding, preserving JSON escaping.
func encodeSavedPlan(p *savedPlan, redact func(string) string) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var v any
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	var scrub func(any) any
	scrub = func(v any) any {
		switch x := v.(type) {
		case string:
			return redact(x)
		case map[string]any:
			for k, v := range x {
				x[k] = scrub(v)
			}
		case []any:
			for i, v := range x {
				x[i] = scrub(v)
			}
		}
		return v
	}
	return json.MarshalIndent(scrub(v), "", "  ")
}
