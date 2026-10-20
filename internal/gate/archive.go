package gate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
)

// ErrArchiveTooLarge: the archive decompresses to more than the limit.
var ErrArchiveTooLarge = errors.New("image archive is larger than the Gate allows")

// ArchiveTags lists the image references a `docker save` archive (docker or
// OCI layout, optionally gzipped) tags when loaded: manifest.json RepoTags,
// the legacy repositories file, and index.json image names. docker load
// tags whatever the archive names, so a Member could otherwise replace
// another App's image (or the Proxy's) on the Server.
//
// It is strict so that what it reads is what docker load reads: only
// regular files and directories, each under one canonical relative name
// (no "/", "..", "." or empty segments, no duplicates), so links and path
// tricks cannot point docker load at metadata this function never saw.
// max bounds the decompressed size (0: no bound).
func ArchiveTags(r io.Reader, max int64) ([]string, error) {
	br := bufio.NewReader(r)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("image archive: %w", err)
		}
		defer gz.Close()
		r = gz
	} else {
		r = br
	}
	if max > 0 {
		r = &limitReader{r: r, n: max}
	}
	const maxMeta = 4 << 20
	var tags []string
	seen := map[string]bool{}
	meta := false
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrArchiveTooLarge) {
				return nil, err
			}
			return nil, fmt.Errorf("read image archive: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if !canonicalName(name) {
			return nil, fmt.Errorf("image archive: entry %q is not a plain relative path", hdr.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("image archive: entry %q appears twice", name)
		}
		seen[name] = true
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeDir:
			continue
		default:
			return nil, fmt.Errorf("image archive: entry %q is a link or special file (type %q); only regular files and directories are allowed", name, string(hdr.Typeflag))
		}
		if name != "manifest.json" && name != "repositories" && name != "index.json" {
			continue
		}
		if hdr.Size > maxMeta {
			return nil, fmt.Errorf("image archive: %s is too large", name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read image archive: %w", err)
		}
		meta = true
		t, err := metadataTags(name, b)
		if err != nil {
			return nil, err
		}
		tags = append(tags, t...)
	}
	if !meta {
		return nil, errors.New("image archive has no manifest.json, repositories or index.json")
	}
	for i, t := range tags {
		tags[i] = NormalizeRef(t)
	}
	slices.Sort(tags)
	return slices.Compact(tags), nil
}

// metadataTags reads the references one metadata file names.
func metadataTags(name string, b []byte) ([]string, error) {
	var tags []string
	switch name {
	case "manifest.json":
		var entries []struct{ RepoTags []string }
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, fmt.Errorf("image archive manifest.json: %w", err)
		}
		for _, e := range entries {
			tags = append(tags, e.RepoTags...)
		}
	case "repositories":
		var repos map[string]map[string]string
		if err := json.Unmarshal(b, &repos); err != nil {
			return nil, fmt.Errorf("image archive repositories: %w", err)
		}
		for repo, ts := range repos {
			for t := range ts {
				tags = append(tags, repo+":"+t)
			}
		}
	case "index.json":
		var idx struct {
			Manifests []struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, fmt.Errorf("image archive index.json: %w", err)
		}
		for _, m := range idx.Manifests {
			full := m.Annotations["io.containerd.image.name"]
			ref := m.Annotations["org.opencontainers.image.ref.name"]
			if full != "" {
				tags = append(tags, full)
			}
			switch {
			case ref == "":
			case strings.ContainsAny(ref, ":/@"):
				tags = append(tags, ref)
			case full == "":
				// A bare tag with no image name: loaders differ on what
				// they name it, so the Gate cannot tell what it would tag.
				return nil, fmt.Errorf("image archive index.json: reference %q has no image name", ref)
			}
		}
	}
	return tags, nil
}

// canonicalName: a relative path with no empty, "." or ".." segment.
func canonicalName(n string) bool {
	if n == "" || strings.HasPrefix(n, "/") || strings.Contains(n, "\\") || strings.ContainsRune(n, 0) {
		return false
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return path.Clean(n) == n
}

type limitReader struct {
	r io.Reader
	n int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		// One byte more than the limit means the archive is too large.
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, ErrArchiveTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// NormalizeRef spells a reference the way Docker resolves it, minus the
// implicit Docker Hub registry: "docker.io/library/postgres" and "postgres"
// are both "postgres:latest", "GHCR.IO/a/b" is "ghcr.io/a/b:latest" (hosts
// are case insensitive), while "library/a/b" (a real repository path) keeps
// its prefix. Tags are case sensitive, so their case is kept.
func NormalizeRef(r string) string {
	r = normalizeHost(r)
	if !strings.Contains(r, "@") && strings.LastIndex(r, ":") <= strings.LastIndex(r, "/") {
		r += ":latest"
	}
	return r
}

// normalizeHost lowercases a reference's registry host and drops Docker
// Hub's (and its library/ namespace for one-segment names). It works on
// repository prefixes too ("GHCR.IO/acme/").
func normalizeHost(r string) string {
	if first, rest, ok := strings.Cut(r, "/"); ok && (strings.ContainsAny(first, ".:") || strings.EqualFold(first, "localhost")) {
		first = strings.ToLower(first)
		if first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io" {
			r = rest
		} else {
			r = first + "/" + rest
		}
	}
	if rest, ok := strings.CutPrefix(r, "library/"); ok && !strings.Contains(nameOf(rest), "/") {
		r = rest
	}
	return r
}

// HasRepoPrefix reports whether ref is under the repository prefix (a
// registry host and/or namespace ending in "/"), both normalized.
func HasRepoPrefix(ref, prefix string) bool {
	return strings.HasPrefix(NormalizeRef(ref), normalizeHost(prefix))
}

// nameOf is a reference without its tag or digest.
func nameOf(r string) string {
	if i := strings.Index(r, "@"); i >= 0 {
		r = r[:i]
	}
	if i := strings.LastIndex(r, ":"); i > strings.LastIndex(r, "/") {
		r = r[:i]
	}
	return r
}
