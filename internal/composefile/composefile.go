// Package composefile loads compose files with compose-go, extracts the
// per-Service `x-yoho` extension and checks them against a runtime.
package composefile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

// DefaultFiles are tried in order in dir only (no walking up to parents, so a
// stray compose file elsewhere is never deployed).
var DefaultFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Result is a loaded compose project plus Yoho data.
type Result struct {
	Project *types.Project
	// Ext is x-yoho per Service name (zero value when absent).
	Ext map[string]config.ServiceExt
	// Raw holds the uninterpolated file contents, for interpolation checks.
	Raw [][]byte
	// EnvFileKeys are the keys found in dir/.env (values not retained).
	EnvFileKeys []string
	// Overlays are Destination compose files included after the base files
	// (name.<destination>.ext). Empty when none exist.
	Overlays []string
}

// Load reads compose files in dir. files are relative to dir; empty means
// default discovery. destination selects optional overlays named
// <file>.<destination><ext> (compose.yaml → compose.staging.yaml). Those
// files are appended after the base files so they override.
//
// .env policy: dir/.env is allowed for NON-secret interpolation values at the
// lowest precedence (env, i.e. Destination.env, wins). Its keys are exposed in
// EnvFileKeys and Check warns when one is also a secret key. The process
// environment is never read, so builds are reproducible and operator secrets
// cannot leak into the compiled compose.
func Load(ctx context.Context, dir string, files []string, env map[string]string, projectName, destination string) (*Result, error) {
	paths, overlays, err := discover(dir, files, destination)
	if err != nil {
		return nil, err
	}
	res := &Result{Ext: map[string]config.ServiceExt{}, Overlays: overlays}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		res.Raw = append(res.Raw, b)
	}
	merged, keys, err := mergeEnv(dir, env)
	if err != nil {
		return nil, err
	}
	res.EnvFileKeys = keys

	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir),
		cli.WithName(projectName),
	)
	if err != nil {
		return nil, err
	}
	opts.Environment = merged
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	res.Project = p

	for name, svc := range p.Services {
		raw, ok := svc.Extensions["x-yoho"]
		if !ok {
			res.Ext[name] = config.ServiceExt{}
			continue
		}
		e, err := config.DecodeServiceExt(raw)
		if err != nil {
			return nil, &ExtError{Service: name, Err: err}
		}
		res.Ext[name] = e
	}
	return res, nil
}

// ExtError is an invalid x-yoho block in a compose file that otherwise
// loaded: an error in the checked files, not a load failure.
type ExtError struct {
	Service string
	Err     error
}

func (e *ExtError) Error() string { return fmt.Sprintf("service %q: x-yoho: %v", e.Service, e.Err) }
func (e *ExtError) Unwrap() error { return e.Err }

// ServiceNames returns the sorted names of the active Services (the ones Yoho
// deploys; profile-disabled Services are excluded) from the same files,
// Destination overlays and env as Load. It is a lenient load for diagnostic
// commands: env_file and label_file are not read, and consistency checks,
// schema validation and x-yoho decoding are skipped.
func ServiceNames(ctx context.Context, dir string, files []string, env map[string]string, projectName, destination string) ([]string, error) {
	paths, _, err := discover(dir, files, destination)
	if err != nil {
		return nil, err
	}
	merged, _, err := mergeEnv(dir, env)
	if err != nil {
		return nil, err
	}
	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir),
		cli.WithName(projectName),
		cli.WithoutEnvironmentResolution,
		cli.WithoutLabelsResolution,
		cli.WithConsistency(false),
		cli.WithResolvedPaths(false),
		cli.WithLoadOptions(loader.WithSkipValidation),
	)
	if err != nil {
		return nil, err
	}
	opts.Environment = merged
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	return p.ServiceNames(), nil
}

// discover resolves the compose files (default discovery when files is
// empty) and the Destination overlays.
func discover(dir string, files []string, destination string) (paths, overlays []string, err error) {
	if len(files) == 0 {
		for _, n := range DefaultFiles {
			if fileExists(filepath.Join(dir, n)) {
				files = []string{n}
				break
			}
		}
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("no compose file in %s (looked for %v)", dir, DefaultFiles)
		}
	}
	paths, overlays = composePaths(dir, files, destination)
	return paths, overlays, nil
}

// mergeEnv returns the interpolation environment: dir/.env at the lowest
// precedence, then env. keys are the sorted .env keys.
func mergeEnv(dir string, env map[string]string) (merged map[string]string, keys []string, err error) {
	merged = map[string]string{}
	if dotenvPath := filepath.Join(dir, ".env"); fileExists(dotenvPath) {
		m, err := dotenv.GetEnvFromFile(map[string]string{}, []string{dotenvPath})
		if err != nil {
			return nil, nil, fmt.Errorf(".env: %w", err)
		}
		for k, v := range m {
			merged[k] = v
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	for k, v := range env {
		merged[k] = v
	}
	return merged, keys, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// composePaths resolves files against dir and appends an existing
// name.<destination>.ext overlay after every base file.
func composePaths(dir string, files []string, destination string) (paths, overlays []string) {
	seen := map[string]bool{}
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, f)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	if destination == "" || strings.ContainsAny(destination, `/\`) {
		return paths, nil
	}
	bases := append([]string(nil), paths...)
	for _, p := range bases {
		ext := filepath.Ext(p)
		ov := strings.TrimSuffix(p, ext) + "." + destination + ext
		if seen[ov] || !fileExists(ov) {
			continue
		}
		seen[ov] = true
		paths = append(paths, ov)
		overlays = append(overlays, ov)
	}
	return paths, overlays
}

// StripBuild sets each Service's image to images[name] (when present) and
// removes its build section, so the compiled compose references only images
// Yoho already built and transported.
func StripBuild(p *types.Project, images map[string]string) {
	for name, svc := range p.Services {
		if img, ok := images[name]; ok {
			svc.Image = img
		}
		svc.Build = nil
		p.Services[name] = svc
	}
}

// LoadResolved loads a project the CLI already resolved and marshalled
// (the Gate's deploy request): no interpolation, includes, extends, env or
// label files, so nothing on this machine is read. workingDir only anchors
// relative paths, which a resolved project does not have.
func LoadResolved(ctx context.Context, content []byte, workingDir, projectName string) (*Result, error) {
	p, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  workingDir,
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: content}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipInterpolation = true
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipResolveEnvironment = true
		o.SkipResolveLabels = true
		o.ResolvePaths = false
	})
	if err != nil {
		return nil, err
	}
	res := &Result{Project: p, Ext: map[string]config.ServiceExt{}}
	for name, svc := range p.Services {
		raw, ok := svc.Extensions["x-yoho"]
		if !ok {
			res.Ext[name] = config.ServiceExt{}
			continue
		}
		e, err := config.DecodeServiceExt(raw)
		if err != nil {
			return nil, &ExtError{Service: name, Err: err}
		}
		res.Ext[name] = e
	}
	return res, nil
}

// Resolved marshals p for LoadResolved: env_file and label_file entries are
// dropped (compose-go already merged them into environment and labels).
func Resolved(p *types.Project) ([]byte, error) {
	q, err := p.WithServicesTransform(func(_ string, s types.ServiceConfig) (types.ServiceConfig, error) {
		s.EnvFiles = nil
		s.LabelFiles = nil
		return s, nil
	})
	if err != nil {
		return nil, err
	}
	return q.MarshalYAML()
}
