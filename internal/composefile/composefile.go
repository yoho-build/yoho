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
	if len(files) == 0 {
		for _, n := range DefaultFiles {
			if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
				files = []string{n}
				break
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("no compose file in %s (looked for %v)", dir, DefaultFiles)
		}
	}
	paths, overlays := composePaths(dir, files, destination)
	res := &Result{Ext: map[string]config.ServiceExt{}, Overlays: overlays}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		res.Raw = append(res.Raw, b)
	}

	merged := map[string]string{}
	if dotenvPath := filepath.Join(dir, ".env"); fileExists(dotenvPath) {
		m, err := dotenv.GetEnvFromFile(map[string]string{}, []string{dotenvPath})
		if err != nil {
			return nil, fmt.Errorf(".env: %w", err)
		}
		for k, v := range m {
			merged[k] = v
			res.EnvFileKeys = append(res.EnvFileKeys, k)
		}
		sort.Strings(res.EnvFileKeys)
	}
	for k, v := range env {
		merged[k] = v
	}

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
			return nil, fmt.Errorf("service %q: x-yoho: %w", name, err)
		}
		res.Ext[name] = e
	}
	return res, nil
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
