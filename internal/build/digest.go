package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// InputsDigest hashes the local build inputs using Docker's ignore rules.
// Only the digest leaves this function; file contents never enter a plan.
func InputsDigest(o Options) (string, error) {
	names := []string{}
	for name, svc := range o.Project.Services {
		if svc.Build != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	h := sha256.New()
	if len(names) == 0 {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	ins, err := allInputs(o, names)
	if err != nil {
		return "", err
	}
	root, _, err := transferRoot(o, ins)
	if err != nil {
		return "", err
	}
	keep, err := keepSet(root, ins, nil)
	if err != nil {
		return "", err
	}
	// BuildKit file secrets are not build inputs for the approval digest.
	blocked := map[string]bool{}
	for _, in := range ins {
		for _, secret := range in.secrets {
			hops, e := inputHops(secret.path, false)
			if e != nil {
				return "", e
			}
			for _, p := range hops {
				blocked[relTo(root, p)] = true
			}
		}
	}
	paths := make([]string, 0, len(keep))
	for p := range keep {
		if blocked[p] {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		st, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, st.Mode())
		if st.IsDir() {
			continue
		}
		var b []byte
		if st.Mode()&os.ModeSymlink != 0 {
			v, e := os.Readlink(full)
			b, err = []byte(v), e
		} else {
			b, err = os.ReadFile(full)
		}
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
