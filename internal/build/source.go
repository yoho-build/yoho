package build

// What a server build ships (ADR 0009): the build inputs of every Service,
// a transfer root that contains them, a keep set computed from the ignore
// files, and an rsync exclude list for everything else under the root.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"

	"github.com/yoho-build/yoho/internal/version"
)

// inputs are one Service's local build inputs. Paths are absolute as
// resolved from the compose file, not yet through symlinks.
type inputs struct {
	service    string
	context    string // "" for a remote context
	dockerfile string // "" when inline or remote
	ignore     string // the ignore file BuildKit uses, "" when none
	dirs       []contextDir
	files      []string // local single-file additional contexts
	secrets    []secretFile
}

// contextDir is a directory Docker sends, filtered by its ignore file.
type contextDir struct{ dir, ignore string }

type secretFile struct{ name, path string }

// resolveLocal resolves a compose path the way compose-go does, against
// Project.WorkingDir.
func resolveLocal(o Options, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(o.baseDir(), p)
}

// isLocalContext reports whether an additional_contexts value is a path.
func isLocalContext(v string) bool {
	for _, pre := range []string{"git@", "github.com/", "service:", "target:"} {
		if strings.HasPrefix(v, pre) {
			return false
		}
	}
	return !strings.Contains(v, "://")
}

// ignoreFileAt reports whether p is an ignore file. A file that exists but
// cannot be read is an error, never "no ignore file": that would ship what
// it excludes.
func ignoreFileAt(p string) (bool, error) {
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("ignore file %s: %w", p, err)
	case st.IsDir():
		return false, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return false, fmt.Errorf("ignore file %s: %w", p, err)
	}
	f.Close()
	return true, nil
}

// ignoreFor is the ignore file BuildKit opens: <Dockerfile>.dockerignore
// next to the Dockerfile, else <context>/.dockerignore.
func ignoreFor(contextDir, dockerfile string) (string, error) {
	var candidates []string
	if dockerfile != "" {
		candidates = append(candidates, dockerfile+".dockerignore")
	}
	for _, p := range append(candidates, filepath.Join(contextDir, ".dockerignore")) {
		ok, err := ignoreFileAt(p)
		if ok || err != nil {
			return p, err
		}
	}
	return "", nil
}

// defaultDockerfile is the file BuildKit loads when no dockerfile is set:
// Dockerfile, else dockerfile. Names are compared exactly, because a
// case-insensitive volume answers a Stat for either.
func defaultDockerfile(contextDir string) string {
	entries, _ := os.ReadDir(contextDir)
	name := "Dockerfile"
	for _, want := range []string{"Dockerfile", "dockerfile"} {
		found := false
		for _, e := range entries {
			found = found || e.Name() == want
		}
		if found {
			name = want
			break
		}
	}
	return filepath.Join(contextDir, name)
}

func resolveInputs(o Options, service string, b *types.BuildConfig) (inputs, error) {
	in := inputs{service: service}
	s := localSpec(o, b)
	if isRemoteContext(s.context) {
		return in, nil
	}
	in.context = s.context
	switch s.dockerfile {
	case "-":
	case "":
		in.dockerfile = defaultDockerfile(s.context)
	default:
		in.dockerfile = s.dockerfile
	}
	var err error
	if in.ignore, err = ignoreFor(s.context, in.dockerfile); err != nil {
		return in, err
	}
	for _, k := range sortedKeys(b.AdditionalContexts) {
		v := b.AdditionalContexts[k]
		if strings.HasPrefix(v, "oci-layout://") {
			return in, fmt.Errorf("additional context %s: oci-layout:// is not supported by builder.location=server\nhint: use builder.location: local or remote", k)
		}
		if !isLocalContext(v) {
			continue
		}
		p := resolveLocal(o, v)
		st, err := os.Stat(p)
		if err != nil {
			return in, fmt.Errorf("additional context %s: %w", k, err)
		}
		if st.IsDir() {
			ignore, err := ignoreFor(p, "")
			if err != nil {
				return in, fmt.Errorf("additional context %s: %w", k, err)
			}
			in.dirs = append(in.dirs, contextDir{dir: p, ignore: ignore})
		} else {
			in.files = append(in.files, p)
		}
	}
	builderSecret := map[string]bool{}
	for _, k := range o.Builder.Secrets {
		builderSecret[k] = true
	}
	for _, sec := range b.Secrets {
		id := sec.Target
		if id == "" {
			id = sec.Source
		}
		def, ok := o.Project.Secrets[sec.Source]
		if builderSecret[id] || !ok || def.Environment != "" || def.File == "" {
			continue // secretArgs reports undefined secrets
		}
		in.secrets = append(in.secrets, secretFile{name: sec.Source, path: resolveLocal(o, def.File)})
	}
	return in, nil
}

func allInputs(o Options, names []string) ([]inputs, error) {
	var all []inputs
	for _, name := range names {
		in, err := resolveInputs(o, name, o.Project.Services[name].Build)
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", name, err)
		}
		all = append(all, in)
	}
	return all, nil
}

// dirPaths lists the input directories: contexts and additional contexts.
func (in inputs) dirPaths() []string {
	var ps []string
	if in.context != "" {
		ps = append(ps, in.context)
	}
	for _, d := range in.dirs {
		ps = append(ps, d.dir)
	}
	return ps
}

// protected lists the files that ship regardless of the gitignore and
// dockerignore rules: Dockerfiles, ignore files in use, file secrets.
func (in inputs) protected() []string {
	var ps []string
	for _, p := range []string{in.dockerfile, in.ignore} {
		if p != "" {
			ps = append(ps, p)
		}
	}
	for _, d := range in.dirs {
		if d.ignore != "" {
			ps = append(ps, d.ignore)
		}
	}
	for _, s := range in.secrets {
		ps = append(ps, s.path)
	}
	return ps
}

// filePaths lists every input file.
func (in inputs) filePaths() []string { return append(in.protected(), in.files...) }

// inputHops is every path an input needs on the Server: a directory's
// physical path, or a file's path as written (its directory resolved) plus
// each link of its symlink chain, ending at the target. BuildKit opens a
// Dockerfile or ignore file by its expected name, so the link must ship
// with what it points to. Only relative links whose path has no directory
// symlink are accepted; others would dangle on the Server.
func inputHops(p string, dir bool) ([]string, error) {
	if dir {
		pp, err := physical(p)
		return []string{pp}, err
	}
	var hops []string
	p, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	for range 40 {
		d, err := physical(filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		q := filepath.Join(d, filepath.Base(p))
		hops = append(hops, q)
		fi, err := os.Lstat(q)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return hops, nil
		}
		target, err := os.Readlink(q)
		if err != nil {
			return nil, err
		}
		// rsync ships links verbatim: an absolute target would point at this
		// machine's path on the Server, and a directory symlink on the way
		// would not ship, so the link would dangle.
		hint := "; use a regular file or a relative symlink with no directory symlinks in its path"
		if filepath.IsAbs(target) {
			return nil, fmt.Errorf("%s: symlink %s has an absolute target %s, which does not resolve on the Server%s", hops[0], q, target, hint)
		}
		// Join cleans lexically: "via/../x" would hide a directory symlink
		// via from the check below.
		if filepath.Clean(target) != target {
			return nil, fmt.Errorf("%s: symlink %s has a non-normalized target %s%s", hops[0], q, target, hint)
		}
		next := filepath.Join(d, target)
		if pd, err := physical(filepath.Dir(next)); err != nil || pd != filepath.Dir(next) {
			return nil, fmt.Errorf("%s: symlink %s points through a directory symlink (%s), which does not ship to the Server%s", hops[0], q, target, hint)
		}
		p = next
	}
	return nil, fmt.Errorf("%s: too many levels of symbolic links", hops[0])
}

// allHops returns every input's hops, each paired with the input as
// written.
func allHops(ins []inputs) (hops, of []string, err error) {
	for _, in := range ins {
		for i, list := range [][]string{in.dirPaths(), in.filePaths()} {
			for _, p := range list {
				hs, err := inputHops(p, i == 0)
				if err != nil {
					return nil, nil, err
				}
				for _, h := range hs {
					hops, of = append(hops, h), append(of, p)
				}
			}
		}
	}
	return hops, of, nil
}

// serverPath is the local path an input maps from on the Server: physical
// for a directory, the name as written for a file (see inputHops).
func serverPath(p string) (string, error) {
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return physical(p)
	}
	hops, err := inputHops(p, false)
	if err != nil {
		return "", err
	}
	return hops[0], nil
}

// physical resolves symlinks in p. A missing tail (a Dockerfile that does
// not exist yet) is kept as written below its deepest existing ancestor.
func physical(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest), nil
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// within reports whether p is dir or below it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func commonAncestor(a, b string) string {
	for !within(a, b) {
		parent := filepath.Dir(a)
		if parent == a {
			return a
		}
		a = parent
	}
	return a
}

// relTo is p relative to root, slash-separated ("." for root itself).
func relTo(root, p string) string {
	rel, _ := filepath.Rel(root, p)
	return filepath.ToSlash(rel)
}

// gitTop returns the physical top level of the git work tree containing
// dir. Any failure (no git, not a work tree, a broken repository or config)
// is an error; callers decide whether they need git.
func gitTop(dir string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", err
	}
	out, err := version.Git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return physical(strings.TrimSpace(string(out)))
}

// transferRoot returns the transfer root (physical) and the git top level
// ("" when git cannot be used). The root is the deepest common ancestor of
// the Yoho file's directory and every input, and must lie inside the work
// tree unless it is the Yoho file's directory itself; then git is optional
// and only feeds the gitignore step.
func transferRoot(o Options, ins []inputs) (root, top string, err error) {
	yoho, err := physical(o.Dir)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(yoho); err != nil {
		return "", "", err
	}
	hops, of, err := allHops(ins)
	if err != nil {
		return "", "", err
	}
	root = yoho
	outside := ""
	for i, h := range hops {
		if outside == "" && !within(yoho, h) {
			outside = of[i]
		}
		root = commonAncestor(root, h)
	}
	top, gerr := gitTop(yoho)
	if root == yoho {
		return root, top, nil
	}
	if gerr != nil {
		return "", "", fmt.Errorf("%s is outside %s and git cannot find a work tree there (%v), so Yoho will not sync %s\nhint: move build inputs inside %s, or deploy from a git repository", outside, o.Dir, gerr, root, o.Dir)
	}
	if !within(top, root) {
		for i, h := range hops {
			if !within(top, h) {
				return "", "", fmt.Errorf("%s is outside the git work tree %s and is not synced to the Server", of[i], top)
			}
		}
		return "", "", fmt.Errorf("transfer root %s is outside the git work tree %s", root, top)
	}
	return root, top, nil
}

// ignoredSet holds paths git ignores, relative to the transfer root.
type ignoredSet struct {
	all   bool // the transfer root itself is ignored
	paths map[string]bool
}

func (s *ignoredSet) has(rel string) bool {
	if s == nil {
		return false
	}
	if s.all {
		return true
	}
	for p := rel; p != "." && p != "/"; p = path.Dir(p) {
		if s.paths[p] {
			return true
		}
	}
	return false
}

// rebase maps a path relative to the git top level onto the transfer root.
// ok is false outside the root; p == "." is the root itself.
func rebase(rootRel, p string) (string, bool) {
	switch {
	case rootRel == ".":
		return p, true
	case p == rootRel:
		return ".", true
	case strings.HasPrefix(p, rootRel+"/"):
		return p[len(rootRel)+1:], true
	}
	return "", false
}

// gitIgnored lists what git ignores, once from the top level, rebased onto
// the transfer root. A reported directory covers everything below it.
func gitIgnored(top, root string) (*ignoredSet, error) {
	out, err := version.Git(top, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", top, err)
	}
	rootRel := relTo(top, root)
	s := &ignoredSet{paths: map[string]bool{}}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		p = strings.TrimSuffix(p, "/")
		if rootRel != "." && strings.HasPrefix(rootRel+"/", p+"/") {
			s.all = true // an ignored directory contains the root
			continue
		}
		if rel, ok := rebase(rootRel, p); ok && rel != "." {
			s.paths[rel] = true
		}
	}
	return s, nil
}

// alwaysExcluded: .git and .yoho/secrets* at any depth never ship.
func alwaysExcluded(rel string) bool {
	base := path.Base(rel)
	return base == ".git" || (strings.HasPrefix(base, "secrets") && path.Base(path.Dir(rel)) == ".yoho")
}

func hasAlwaysExcluded(rel string) bool {
	for p := rel; p != "." && p != "/"; p = path.Dir(p) {
		if alwaysExcluded(p) {
			return true
		}
	}
	return false
}

// readIgnore parses a dockerignore file. Errors name the file and line.
func readIgnore(p string) (*patternmatcher.PatternMatcher, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	pats, err := ignorefile.ReadAll(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	pm, err := patternmatcher.New(pats)
	if err == nil {
		return pm, nil
	}
	for i, line := range strings.Split(string(b), "\n") {
		one, _ := ignorefile.ReadAll(strings.NewReader(line))
		if _, lerr := patternmatcher.New(one); lerr != nil {
			return nil, fmt.Errorf("%s:%d: %w", p, i+1, lerr)
		}
	}
	return nil, fmt.Errorf("%s: %w", p, err)
}

// reincludeBelow reports whether a ! pattern could match a path below dir,
// as BuildKit's fsutil walker decides it: a pattern with a wildcard (other
// than a trailing /** or /*) always could; otherwise its literal path must
// lie below dir.
func reincludeBelow(pm *patternmatcher.PatternMatcher, dir string) bool {
	if !pm.Exclusions() {
		return false
	}
	for _, pat := range pm.Patterns() {
		if !pat.Exclusion() {
			continue
		}
		lit := filepath.ToSlash(pat.String())
		lit = strings.TrimSuffix(strings.TrimSuffix(lit, "/**"), "/*")
		if strings.ContainsAny(lit, `*?[\`) || strings.HasPrefix(lit+"/", dir+"/") {
			return true
		}
	}
	return false
}

// walkContext adds what Docker would send from dir to keep, the way
// BuildKit's walker applies the ignore file: MatchInfo threads from each
// directory to its children, and an excluded directory is skipped unless a
// ! pattern could re-include something below it. A permission error under
// an excluded directory is ignored, as fsutil does; anywhere else it fails
// the build. prune drops paths before matching (git-ignored, .git,
// .yoho/secrets*).
func walkContext(root, dir, ignore string, keep map[string]bool, prune func(rel string) bool) error {
	var pm *patternmatcher.PatternMatcher
	if ignore != "" {
		var err error
		if pm, err = readIgnore(ignore); err != nil {
			return err
		}
	}
	base := relTo(root, dir)
	if prune(base) {
		return nil
	}
	keep[base] = true
	infos := map[string]patternmatcher.MatchInfo{}
	excludedDirs := map[string]bool{}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		rel := relTo(dir, p)
		if err != nil {
			// WalkDir reports an unreadable directory after visiting it.
			if rel != "." && excludedDirs[rel] && errors.Is(err, fs.ErrPermission) {
				return filepath.SkipDir
			}
			return fmt.Errorf("build context: %w", err)
		}
		if rel == "." {
			return nil
		}
		rr := path.Join(base, rel)
		if prune(rr) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if pm != nil {
			excluded, info, err := pm.MatchesUsingParentResults(rel, infos[path.Dir(rel)])
			if err != nil {
				return fmt.Errorf("%s: %w", ignore, err)
			}
			if d.IsDir() {
				infos[rel] = info
			}
			if excluded {
				if d.IsDir() {
					if !reincludeBelow(pm, rel) {
						return filepath.SkipDir
					}
					excludedDirs[rel] = true
				}
				return nil
			}
		}
		keep[rr] = true
		return nil
	})
}

// keepSet is every path (relative to root) that ships, ancestors included.
func keepSet(root string, ins []inputs, ignored *ignoredSet) (map[string]bool, error) {
	keep := map[string]bool{}
	prune := func(rel string) bool { return alwaysExcluded(rel) || ignored.has(rel) }
	walk := func(dir, ignore string) error {
		pd, err := physical(dir)
		if err != nil {
			return err
		}
		return walkContext(root, pd, ignore, keep, prune)
	}
	for _, in := range ins {
		if in.context == "" {
			continue
		}
		if err := walk(in.context, in.ignore); err != nil {
			return nil, err
		}
		for _, d := range in.dirs {
			if err := walk(d.dir, d.ignore); err != nil {
				return nil, err
			}
		}
		// A single-file context ships unless its name is dropped; then its
		// whole symlink chain ships, so the link resolves on the Server.
		for _, f := range in.files {
			hops, err := inputHops(f, false)
			if err != nil {
				return nil, err
			}
			if rel := relTo(root, hops[0]); prune(rel) || hasAlwaysExcluded(rel) {
				continue
			}
			for _, h := range hops {
				if rel := relTo(root, h); !hasAlwaysExcluded(rel) {
					keep[rel] = true
				}
			}
		}
		// Dockerfiles, ignore files and file secrets ship regardless of the
		// gitignore and dockerignore rules, with every link of a symlink
		// chain; .git and .yoho/secrets* still win.
		for _, p := range in.protected() {
			hops, err := inputHops(p, false)
			if err != nil {
				return nil, err
			}
			for _, h := range hops {
				rel := relTo(root, h)
				if hasAlwaysExcluded(rel) {
					return nil, fmt.Errorf("%s is under .git or .yoho/secrets*, which never ship to the Server", h)
				}
				keep[rel] = true
			}
		}
	}
	for rel := range keep {
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			if keep[d] {
				break
			}
			keep[d] = true
		}
	}
	keep["."] = true
	return keep, nil
}

// excludeList walks root and returns one anchored, escaped rsync exclude per
// maximal subtree with nothing kept and per file not kept, plus the number
// and total size (Lstat) of the files that ship.
func excludeList(root string, keep map[string]bool) (excl []string, files int, size int64, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relTo(root, p)
		if rel == "." {
			return nil
		}
		if keep[rel] && !alwaysExcluded(rel) {
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				files++
				size += info.Size()
			}
			return nil
		}
		if d.IsDir() {
			excl = append(excl, "/"+rsyncEscape(rel)+"/")
			return filepath.SkipDir
		}
		excl = append(excl, "/"+rsyncEscape(rel))
		return nil
	})
	return excl, files, size, err
}

// caseCollisions fails when two kept paths in the git index differ only by
// case: a case-insensitive volume shows one of them, so a walk cannot tell.
func caseCollisions(top, root string, keep map[string]bool) error {
	out, err := version.Git(top, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("git ls-files in %s: %w", top, err)
	}
	folded := make(map[string]bool, len(keep))
	for rel := range keep {
		folded[strings.ToLower(rel)] = true
	}
	rootRel := relTo(top, root)
	seen := map[string]string{}
	for _, p := range strings.Split(string(out), "\x00") {
		rel, ok := rebase(rootRel, p)
		if p == "" || !ok || rel == "." || !folded[strings.ToLower(rel)] {
			continue
		}
		for q := rel; q != "." && q != "/"; q = path.Dir(q) {
			k := strings.ToLower(q)
			if prev, ok := seen[k]; ok && prev != q {
				return fmt.Errorf("%s and %s differ only by case; a case-insensitive file system keeps one of them, so the Server would not get both\nhint: rename one of them in git", prev, q)
			}
			seen[k] = q
		}
	}
	return nil
}

// sourcePlan is what a server build ships.
type sourcePlan struct {
	root     string // transfer root, physical
	excludes []string
	files    int
	bytes    int64
}

func planSource(o Options, names []string) (*sourcePlan, error) {
	ins, err := allInputs(o, names)
	if err != nil {
		return nil, err
	}
	root, top, err := transferRoot(o, ins)
	if err != nil {
		return nil, err
	}
	var ignored *ignoredSet
	if top != "" && o.Builder.HonorGitignore() {
		if ignored, err = gitIgnored(top, root); err != nil {
			return nil, err
		}
	}
	keep, err := keepSet(root, ins, ignored)
	if err != nil {
		return nil, err
	}
	if top != "" {
		if err := caseCollisions(top, root, keep); err != nil {
			return nil, err
		}
	}
	excl, files, size, err := excludeList(root, keep)
	if err != nil {
		return nil, err
	}
	return &sourcePlan{root: root, excludes: excl, files: files, bytes: size}, nil
}

// SourceWarning is a `yoho config check` finding about a server build.
type SourceWarning struct {
	Service string
	Message string
}

// SourceWarnings checks the server build inputs of o.Project without
// syncing: the error is what a deploy would fail with.
func SourceWarnings(o Options) ([]SourceWarning, error) {
	if o.Dir == "" {
		o.Dir = o.Project.WorkingDir
	}
	var names []string
	for name, svc := range o.Project.Services {
		if svc.Build != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	ins, err := allInputs(o, names)
	if err != nil {
		return nil, err
	}
	root, _, err := transferRoot(o, ins)
	if err != nil {
		return nil, err
	}
	yoho, err := physical(o.Dir)
	if err != nil {
		return nil, err
	}
	// Every file secret ships, so check each against every directory
	// context of every Service, not only its own Service's.
	var secrets []secretFile
	seen := map[string]bool{}
	for _, in := range ins {
		for _, sec := range in.secrets {
			if !seen[sec.path] {
				seen[sec.path] = true
				secrets = append(secrets, sec)
			}
		}
	}
	var out []SourceWarning
	for _, in := range ins {
		if in.context == "" {
			continue
		}
		if root != yoho && in.ignore == "" {
			out = append(out, SourceWarning{in.service, fmt.Sprintf("server build syncs from %s and context %s has no .dockerignore; everything under it that git does not ignore ships to the Server", root, in.context)})
		}
		for _, c := range append([]contextDir{{dir: in.context, ignore: in.ignore}}, in.dirs...) {
			ws, err := secretsInContext(c, secrets)
			if err != nil {
				return nil, err
			}
			for _, w := range ws {
				out = append(out, SourceWarning{in.service, w})
			}
		}
	}
	return out, nil
}

// secretsInContext names the file secrets Docker would send with context c
// because its ignore file does not exclude them.
func secretsInContext(c contextDir, secrets []secretFile) ([]string, error) {
	pc, err := physical(c.dir)
	if err != nil {
		return nil, err
	}
	var pm *patternmatcher.PatternMatcher
	if c.ignore != "" {
		if pm, err = readIgnore(c.ignore); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, sec := range secrets {
		hops, err := inputHops(sec.path, false)
		if err != nil {
			continue
		}
		for _, h := range hops {
			if !within(pc, h) {
				continue
			}
			excluded := false
			if pm != nil {
				excluded, _ = pm.MatchesOrParentMatches(relTo(pc, h))
			}
			if !excluded {
				out = append(out, fmt.Sprintf("build secret %s (%s) is inside build context %s and no .dockerignore excludes it; Docker would also add it to the context", sec.name, sec.path, c.dir))
				break
			}
		}
	}
	return out, nil
}
