package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/yoho-dev/yoho/internal/config"
)

// Store holds resolved secret values for one Destination. Values exist only
// in memory on the operator's machine.
type Store struct {
	values map[string]string
	refs   map[string]string
}

// New returns a Store from literal values, with ref "inline" (tests, callers
// that already hold values).
func New(values map[string]string) *Store {
	s := &Store{values: map[string]string{}, refs: map[string]string{}}
	for k, v := range values {
		s.values[k], s.refs[k] = v, "inline"
	}
	return s
}

// Get returns the value of key.
func (s *Store) Get(key string) (string, bool) {
	v, ok := s.values[key]
	return v, ok
}

// Keys returns all keys, sorted.
func (s *Store) Keys() []string {
	keys := make([]string, 0, len(s.values))
	for k := range s.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Ref returns where key came from, for audit: "<provider>:<ref>" or
// "file:.yoho/secrets[.<destination>]". Never the value.
func (s *Store) Ref(key string) string { return s.refs[key] }

// LoadOptions tunes Load. The zero value is ready to use.
type LoadOptions struct {
	// Runner executes provider CLIs and $(...) substitutions. Default ExecRunner.
	Runner Runner
	// Maximum concurrent provider calls. Default 4.
	Concurrency int
	// Environment lookup for $VAR fallback and BWS_ACCESS_TOKEN. Default os.LookupEnv.
	Getenv func(string) (string, bool)
	// Warn receives non-fatal notices (overridden keys). Default: dropped.
	Warn func(msg string)
}

// FileNames returns the secrets files read for destination, in precedence
// order (later wins), relative to the App directory.
func FileNames(destination string) []string {
	names := []string{filepath.Join(".yoho", "secrets")}
	if destination != "" {
		names = append(names, filepath.Join(".yoho", "secrets."+destination))
	}
	return names
}

// FileKeys lists keys defined by the secrets files without running any
// $(...) command, for config inspection.
func FileKeys(dir, destination string) ([]string, error) {
	entries, err := readFiles(dir, destination)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var keys []string
	for _, e := range entries {
		if !seen[e.key] {
			seen[e.key] = true
			keys = append(keys, e.key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func readFiles(dir, destination string) ([]fileEntry, error) {
	var all []fileEntry
	for _, name := range FileNames(destination) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries, err := parseDotenv(filepath.ToSlash(name), data)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}
	return all, nil
}

// Load resolves the secrets of destination. Precedence, lowest first:
// provider values from cfg, then .yoho/secrets, then
// .yoho/secrets.<destination>. An explicit file entry wins over a provider
// value with the same key (with a warning) and that provider ref is not read.
//
// The secrets files are trusted code: $(...) runs through `sh -c` with the
// keys defined so far (provider values included) in its environment. Load is
// the only place that happens; use FileKeys for inspection.
func Load(ctx context.Context, dir, destination string, cfg config.SecretsConfig, opts LoadOptions) (*Store, error) {
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Getenv == nil {
		opts.Getenv = os.LookupEnv
	}
	if opts.Warn == nil {
		opts.Warn = func(string) {}
	}
	run := &memoRunner{r: opts.Runner, calls: map[string]*memoCall{}}

	entries, err := readFiles(dir, destination)
	if err != nil {
		return nil, err
	}
	fileKey := map[string]string{}
	for _, e := range entries {
		if prev, ok := fileKey[e.key]; ok && prev == e.file {
			opts.Warn(fmt.Sprintf("%s:%d: %s is defined more than once; the last one wins", e.file, e.line, e.key))
		}
		fileKey[e.key] = e.file
	}

	s := &Store{values: map[string]string{}, refs: map[string]string{}}
	if err := resolveProviders(ctx, s, destination, cfg, fileKey, run, opts); err != nil {
		return nil, err
	}

	// Files are evaluated in order so later entries can expand earlier ones.
	for _, e := range entries {
		v, err := evalEntry(ctx, e, s.values, run, opts.Getenv)
		if err != nil {
			return nil, err
		}
		s.values[e.key], s.refs[e.key] = v, "file:"+e.file
	}
	return s, nil
}

func resolveProviders(ctx context.Context, s *Store, destination string, cfg config.SecretsConfig, fileKey map[string]string, run Runner, opts LoadOptions) error {
	type job struct {
		key, provider, ref string
		p                  config.SecretProvider
	}
	var jobs []job
	var errs []error
	keys := make([]string, 0, len(cfg.Values))
	for k := range cfg.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sv := cfg.Values[k]
		if len(sv.Destinations) > 0 && !slices.Contains(sv.Destinations, destination) {
			continue
		}
		if f, ok := fileKey[k]; ok {
			opts.Warn(fmt.Sprintf("secret %s is set in %s and by provider %q; using %s", k, f, sv.Provider, f))
			continue
		}
		p, ok := cfg.Providers[sv.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("secret %s: unknown provider %q", k, sv.Provider))
			continue
		}
		jobs = append(jobs, job{k, sv.Provider, sv.Ref, p})
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	res := &providerResolver{run: run, getenv: opts.Getenv}
	vals := make([]string, len(jobs))
	jobErrs := make([]error, len(jobs))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := res.resolve(ctx, j.p, j.ref)
			if err != nil {
				jobErrs[i] = fmt.Errorf("secret %s: provider %q (%s) ref %q: %w", j.key, j.provider, j.p.Type, j.ref, err)
				return
			}
			vals[i] = v
		}()
	}
	wg.Wait()
	if err := errors.Join(jobErrs...); err != nil {
		return err
	}
	for i, j := range jobs {
		s.values[j.key], s.refs[j.key] = vals[i], j.provider+":"+j.ref
	}
	return nil
}

func evalEntry(ctx context.Context, e fileEntry, defined map[string]string, run Runner, getenv func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for _, p := range e.parts {
		switch p.kind {
		case litPart:
			b.WriteString(p.s)
		case varPart:
			if v, ok := defined[p.s]; ok {
				b.WriteString(v)
			} else if v, ok := getenv(p.s); ok {
				b.WriteString(v)
			}
		case cmdPart:
			env := make([]string, 0, len(defined))
			known := make([]string, 0, len(defined))
			for k, v := range defined {
				env = append(env, k+"="+v)
				known = append(known, v)
			}
			sort.Strings(env)
			out, stderr, err := run.Run(ctx, []string{"sh", "-c", p.s}, env)
			if err != nil {
				return "", fmt.Errorf("%s:%d: %s: %w", e.file, e.line, e.key, commandError("$(...)", err, out, stderr, known))
			}
			b.WriteString(strings.TrimRight(string(out), "\n"))
		}
	}
	return b.String(), nil
}
