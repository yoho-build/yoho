package dist

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// ErrProvenance wraps every build provenance failure: missing attestation,
// policy mismatch, or an unreachable trust service. Download never falls
// back to checksum-only after it.
var ErrProvenance = errors.New("build provenance verification failed")

// The policy matches `gh attestation verify` in scripts/install.sh.
const (
	provenanceIssuer    = "https://token.actions.githubusercontent.com"
	provenancePredicate = "https://slsa.dev/provenance/v1"
	provenanceRepoURI   = "https://github.com/" + Repo
	provenanceWorkflow  = provenanceRepoURI + "/.github/workflows/release.yml"
)

// maxBundle bounds the attestation list and one decoded bundle.
const maxBundle = 16 << 20

// verifyProvenance checks signed build provenance for a release asset by
// its sha256 digest. Download calls it for fresh and cached files alike.
// Tests replace it.
var verifyProvenance = verifyReleaseProvenance

// trustedMaterial returns the Sigstore public-good trusted root. Tests
// replace it with a snapshot so they run offline.
var trustedMaterial = publicGoodTrustedRoot

var (
	trustedRootMu  sync.Mutex
	trustedRootVal root.TrustedMaterial
)

// publicGoodTrustedRoot fetches trusted_root.json through Sigstore's TUF
// repository, starting from the root embedded in sigstore-go, so key
// rotation is followed. Metadata is cached under UserCacheDir/yoho/sigstore
// and refreshed daily. A success is reused for the process; a failure is not.
func publicGoodTrustedRoot(ctx context.Context) (root.TrustedMaterial, error) {
	trustedRootMu.Lock()
	defer trustedRootMu.Unlock()
	if trustedRootVal != nil {
		return trustedRootVal, nil
	}
	opts := tuf.DefaultOptions().WithCacheValidity(1).WithContext(ctx)
	if d, err := os.UserCacheDir(); err == nil {
		opts = opts.WithCachePath(filepath.Join(d, "yoho", "sigstore"))
	} else {
		opts = opts.WithDisableLocalCache()
	}
	tr, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("load Sigstore trusted root: %w", err)
	}
	trustedRootVal = tr
	return tr, nil
}

type ghAttestation struct {
	Bundle    json.RawMessage `json:"bundle"`
	BundleURL string          `json:"bundle_url"`
}

type ghAttestations struct {
	Attestations []ghAttestation `json:"attestations"`
}

// verifyReleaseProvenance fetches the attestations GitHub stores for digest
// and accepts when any bundle satisfies the release policy for version.
// token, when set, is sent to the API only, never to bundle_url.
func verifyReleaseProvenance(ctx context.Context, token, version string, digest []byte) error {
	if len(digest) != 32 {
		return errors.New("invalid sha256 digest")
	}
	tm, err := trustedMaterial(ctx)
	if err != nil {
		return err
	}
	bundles, err := fetchAttestations(ctx, token, version, hex.EncodeToString(digest))
	if err != nil {
		return err
	}
	policy := releasePolicy(version)
	var errs []error
	for _, b := range bundles {
		err := verifyBundle(tm, b, policy, digest)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("no attestation matches the release policy: %w", errors.Join(errs...))
}

func attestationsURL(hexDigest string) string {
	return strings.TrimRight(APIBase, "/") + "/repos/" + Repo + "/attestations/sha256:" + hexDigest
}

// fetchAttestations returns raw bundle JSON for each attestation. The
// inline bundle is preferred; otherwise bundle_url is fetched without
// Authorization (it is a storage host, not GitHub) and may be
// snappy-compressed, as `gh` handles it.
func fetchAttestations(ctx context.Context, token, version, hexDigest string) ([][]byte, error) {
	noAttestation := fmt.Errorf("release %s has no build provenance attestation for this binary (releases before v0.1.5 have none)", version)
	body, err := getLimit(ctx, attestationsURL(hexDigest), token, acceptAPI, maxBundle)
	if errors.Is(err, errNotFound) {
		return nil, noAttestation
	}
	if err != nil {
		return nil, err
	}
	var list ghAttestations
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errors.New("attestations: invalid response")
	}
	var out [][]byte
	for _, a := range list.Attestations {
		if len(a.Bundle) > 0 && !bytes.Equal(a.Bundle, []byte("null")) {
			out = append(out, a.Bundle)
			continue
		}
		if a.BundleURL == "" {
			continue
		}
		b, err := fetchBundleURL(ctx, a.BundleURL)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, noAttestation
	}
	return out, nil
}

// fetchBundleURL downloads a bundle from a signed storage URL. The URL's
// query carries a signature, so errors name only scheme, host and path.
func fetchBundleURL(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("attestation bundle_url is not an http(s) URL")
	}
	shown := u.Scheme + "://" + u.Host + u.Path
	body, err := getLimit(ctx, rawURL, "", "", maxBundle)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("download %s: not found", shown)
		}
		return nil, errors.New(strings.ReplaceAll(err.Error(), rawURL, shown))
	}
	if json.Valid(body) {
		return body, nil
	}
	dec, err := snappyDecode(body, maxBundle)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", shown, err)
	}
	return dec, nil
}

// provenancePolicy is what a bundle must prove about a release binary.
type provenancePolicy struct {
	issuer, san, repoURI, ref, predicate string
}

// releasePolicy matches `gh attestation verify` in scripts/install.sh: the
// release workflow of Repo, signed through GitHub Actions OIDC at the exact
// tag, with SLSA v1 provenance.
func releasePolicy(version string) provenancePolicy {
	ref := "refs/tags/" + version
	return provenancePolicy{
		issuer:    provenanceIssuer,
		san:       provenanceWorkflow + "@" + ref,
		repoURI:   provenanceRepoURI,
		ref:       ref,
		predicate: provenancePredicate,
	}
}

// verifyBundle checks one Sigstore bundle against p: public-good Fulcio
// chain with an SCT, a Rekor entry and observer timestamp, the certificate
// identity, an in-toto subject equal to digest, and the predicate type.
func verifyBundle(tm root.TrustedMaterial, raw []byte, p provenancePolicy, digest []byte) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("parse bundle: %w", err)
	}
	v, err := verify.NewVerifier(tm,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return err
	}
	id, err := verify.NewCertificateIdentity(
		verify.SubjectAlternativeNameMatcher{SubjectAlternativeName: p.san},
		verify.IssuerMatcher{Issuer: p.issuer},
		certificate.Extensions{SourceRepositoryURI: p.repoURI, SourceRepositoryRef: p.ref},
	)
	if err != nil {
		return err
	}
	res, err := v.Verify(&b, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest),
		verify.WithCertificateIdentity(id),
	))
	if err != nil {
		return err
	}
	if res.Statement == nil || res.Statement.GetPredicateType() != p.predicate {
		return fmt.Errorf("predicate type is not %s", p.predicate)
	}
	return nil
}

// snappyDecode decodes the snappy block format GitHub uses for bundle_url
// (".json.sn"). The output is still verified cryptographically, so a
// decoding bug can only reject, never accept.
func snappyDecode(src []byte, limit int) ([]byte, error) {
	errCorrupt := errors.New("corrupt snappy bundle")
	n, k := binary.Uvarint(src)
	if k <= 0 || n > uint64(limit) {
		return nil, errCorrupt
	}
	src = src[k:]
	dst := make([]byte, 0, n)
	for len(src) > 0 {
		tag := src[0]
		var length, offset int
		switch tag & 3 {
		case 0: // literal
			length = int(tag >> 2)
			src = src[1:]
			if length >= 60 {
				w := length - 59
				if len(src) < w {
					return nil, errCorrupt
				}
				var l uint32
				for i := w - 1; i >= 0; i-- {
					l = l<<8 | uint32(src[i])
				}
				src = src[w:]
				if l >= uint32(limit) {
					return nil, errCorrupt
				}
				length = int(l)
			}
			length++
			if length > len(src) || len(dst)+length > int(n) {
				return nil, errCorrupt
			}
			dst = append(dst, src[:length]...)
			src = src[length:]
			continue
		case 1:
			if len(src) < 2 {
				return nil, errCorrupt
			}
			length = 4 + int(tag>>2&7)
			offset = int(tag>>5)<<8 | int(src[1])
			src = src[2:]
		case 2:
			if len(src) < 3 {
				return nil, errCorrupt
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint16(src[1:3]))
			src = src[3:]
		case 3:
			if len(src) < 5 {
				return nil, errCorrupt
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint32(src[1:5]))
			src = src[5:]
		}
		if offset <= 0 || offset > len(dst) || len(dst)+length > int(n) {
			return nil, errCorrupt
		}
		start := len(dst) - offset
		for i := 0; i < length; i++ {
			dst = append(dst, dst[start+i])
		}
	}
	if len(dst) != int(n) {
		return nil, errCorrupt
	}
	return dst, nil
}
