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
	"time"

	"github.com/yoho-build/yoho/internal/config"
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
	// Warn receives non-fatal notices (overridden keys, retries). Default: dropped.
	Warn func(msg string)
	// OnWait is called once for each provider or $(...) command that runs
	// longer than WaitAfter, usually an approval prompt nobody answered yet.
	// It may run on another goroutine. Events carry names, never values.
	OnWait func(WaitEvent)
	// WaitAfter is how long a command runs before OnWait fires. Default 3 s.
	WaitAfter time.Duration
	// Retries is how many times a provider call that failed with
	// ApprovalTimeout is run again. Default 0.
	Retries int
	// Clock times provider calls for classification. Default time.Now.
	Clock func() time.Time
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
	if err := errors.Join(config.ValidateNonempty(cfg.Nonempty)...); err != nil {
		return nil, err
	}
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
	if opts.WaitAfter <= 0 {
		opts.WaitAfter = 3 * time.Second
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
	ops, err := resolveProviders(ctx, s, destination, cfg, fileKey, fileOpWants(ctx, entries, cfg), run, opts)
	if err != nil {
		return nil, err
	}

	// Files are evaluated in order so later entries can expand earlier ones.
	for _, e := range entries {
		v, err := evalEntry(ctx, e, s.values, run, ops, opts)
		if err != nil {
			if len(cfg.Nonempty) > 0 {
				return nil, contractResolutionError(ctx, e.key)
			}
			return nil, err
		}
		s.values[e.key], s.refs[e.key] = v, "file:"+e.file
	}
	var errs []error
	for _, key := range cfg.Nonempty {
		if value, ok := s.Get(key); !ok || value == "" {
			errs = append(errs, fmt.Errorf("secret %s: required nonempty source value is missing or empty", key))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
}

// contractResolutionError deliberately does not wrap a provider/command error:
// neither stderr nor the error text can be assumed free of unknown credentials.
// Only a context sentinel is safe to preserve for cancellation-aware callers.
func contractResolutionError(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret %s: resolution failed: %w", key, err)
	}
	return fmt.Errorf("secret %s: resolution failed", key)
}

// fileOpWants lists the $(op read ...) substitutions of the secrets files for
// the op batch. None when a key could set OP_* in their environment, which
// would change how op read resolves.
func fileOpWants(ctx context.Context, entries []fileEntry, cfg config.SecretsConfig) []opWant {
	for _, e := range entries {
		if strings.HasPrefix(e.key, "OP_") {
			return nil
		}
	}
	for k := range cfg.Values {
		if strings.HasPrefix(k, "OP_") {
			return nil
		}
	}
	var wants []opWant
	for _, e := range entries {
		for _, p := range e.parts {
			if p.kind != cmdPart {
				continue
			}
			r, ok := parseOpRead(p.s)
			if !ok {
				continue
			}
			wants = append(wants, opWant{opRef: r, key: e.key, fail: func(err error) error {
				if len(cfg.Nonempty) > 0 {
					return contractResolutionError(ctx, e.key)
				}
				return fmt.Errorf("%s:%d: %s: %w", e.file, e.line, e.key, err)
			}})
		}
	}
	return wants
}

// resolveProviders fills s from the provider values in cfg. It first reads
// all op references, its own and fileWants, in one batch per account, and
// returns those values for the $(...) substitutions.
func resolveProviders(ctx context.Context, s *Store, destination string, cfg config.SecretsConfig, fileKey map[string]string, fileWants []opWant, run Runner, opts LoadOptions) (opCache, error) {
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
		return nil, errors.Join(errs...)
	}

	res := &providerResolver{run: run, getenv: opts.Getenv, clock: opts.Clock, retries: opts.Retries,
		onWait: opts.OnWait, waitAfter: opts.WaitAfter, warn: opts.Warn}
	var wants []opWant
	for _, j := range jobs {
		if j.p.Type == "op" {
			wants = append(wants, opWant{opRef: opRef{j.p.Account, j.ref}, key: j.key, fail: func(err error) error {
				return providerJobError(cfg, j.key, j.provider, j.p.Type, j.ref, err, ctx)
			}})
		}
	}
	ops, err := res.opPrefetch(ctx, append(wants, fileWants...))
	if err != nil {
		return nil, err
	}
	res.ops = ops
	vals := make([]string, len(jobs))
	jobErrs := make([]error, len(jobs))
	resolve := func(i int) {
		j := jobs[i]
		v, err := res.resolve(ctx, j.key, j.p, j.ref)
		if err != nil {
			jobErrs[i] = providerJobError(cfg, j.key, j.provider, j.p.Type, j.ref, err, ctx)
			return
		}
		vals[i] = v
	}
	// The first call runs alone: if the provider needs an approval, one
	// prompt then covers the rest instead of several at once. When that
	// call shows the provider is unusable (no session, unanswered prompt),
	// the rest would only repeat it, so stop.
	if len(jobs) > 0 {
		resolve(0)
		var pe *ProviderError
		if errors.As(jobErrs[0], &pe) && (pe.Class == ApprovalTimeout || pe.Class == NotSignedIn) {
			return nil, jobErrs[0]
		}
	}
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i := 1; i < len(jobs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resolve(i)
		}()
	}
	wg.Wait()
	if err := errors.Join(jobErrs...); err != nil {
		return nil, err
	}
	for i, j := range jobs {
		s.values[j.key], s.refs[j.key] = vals[i], j.provider+":"+j.ref
	}
	return ops, nil
}

// providerJobError wraps a failed provider call. Under a secrets.nonempty
// contract all command diagnostics are suppressed, because a provider can
// share a memoized call with a declared key or emit credentials not yet known
// to the Store; only the fixed text of a classified ProviderError survives.
func providerJobError(cfg config.SecretsConfig, key, provider, typ, ref string, err error, ctx context.Context) error {
	if len(cfg.Nonempty) == 0 {
		return fmt.Errorf("secret %s: provider %q (%s) ref %q: %w", key, provider, typ, ref, err)
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.Class != Other {
		return fmt.Errorf("secret %s: resolution failed: %w", key, pe)
	}
	return contractResolutionError(ctx, key)
}

func evalEntry(ctx context.Context, e fileEntry, defined map[string]string, run Runner, ops opCache, opts LoadOptions) (string, error) {
	getenv := opts.Getenv
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
			if r, ok := parseOpRead(p.s); ok {
				if v, ok := ops.get(r); ok {
					b.WriteString(strings.TrimRight(v, "\n"))
					continue
				}
			}
			env := make([]string, 0, len(defined))
			known := make([]string, 0, len(defined))
			for k, v := range defined {
				env = append(env, k+"="+v)
				known = append(known, v)
			}
			sort.Strings(env)
			stop := watchWait(opts.OnWait, opts.WaitAfter, WaitEvent{Provider: "command", Key: e.key})
			out, stderr, err := run.Run(ctx, []string{"sh", "-c", p.s}, env)
			stop()
			if err != nil {
				return "", fmt.Errorf("%s:%d: %s: %w", e.file, e.line, e.key, commandError("$(...)", err, out, stderr, known))
			}
			b.WriteString(strings.TrimRight(string(out), "\n"))
		}
	}
	return b.String(), nil
}
