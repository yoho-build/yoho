package dist

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The v0.1.5 yoho-linux-amd64 Release asset. The binary is not committed;
// tests verify the testdata bundle by this digest.
const (
	fixtureVersion = "v0.1.5"
	fixtureDigest  = "4262dec754ef0c785bd2d522355e4ec0997fa6a614e1a59a6b41715603b6a099"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// useFixtureRoot replaces the TUF lookup with the testdata snapshot of the
// Sigstore public-good trusted root.
func useFixtureRoot(t *testing.T) {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(readFixture(t, "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	prev := trustedMaterial
	trustedMaterial = func(context.Context) (root.TrustedMaterial, error) { return tr, nil }
	t.Cleanup(func() { trustedMaterial = prev })
}

func useAPI(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = prev })
	return srv
}

func mustDigest(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyBundleFixture(t *testing.T) {
	useFixtureRoot(t)
	tm, _ := trustedMaterial(context.Background())
	raw := readFixture(t, "v0.1.5-linux-amd64.sigstore.json")
	digest := mustDigest(t, fixtureDigest)

	if err := verifyBundle(tm, raw, releasePolicy(fixtureVersion), digest); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	wrong := bytes.Clone(digest)
	wrong[0] ^= 0xff
	if err := verifyBundle(tm, raw, releasePolicy(fixtureVersion), wrong); err == nil {
		t.Fatal("wrong digest verified")
	}
	if err := verifyBundle(tm, raw, releasePolicy("v0.1.4"), digest); err == nil || !strings.Contains(err.Error(), "refs/tags/v0.1.4") {
		t.Fatalf("wrong tag: %v", err)
	}
}

func TestVerifyBundleIdentityPolicy(t *testing.T) {
	useFixtureRoot(t)
	tm, _ := trustedMaterial(context.Background())
	raw := readFixture(t, "v0.1.5-linux-amd64.sigstore.json")
	digest := mustDigest(t, fixtureDigest)

	if err := verifyBundle(tm, raw, releasePolicy(fixtureVersion), digest); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for name, set := range map[string]func(*provenancePolicy){
		"repo":      func(p *provenancePolicy) { p.repoURI = "https://github.com/other/yoho" },
		"ref":       func(p *provenancePolicy) { p.ref = "refs/heads/main" },
		"workflow":  func(p *provenancePolicy) { p.san = provenanceRepoURI + "/.github/workflows/other.yml@" + p.ref },
		"issuer":    func(p *provenancePolicy) { p.issuer = "https://example.com" },
		"predicate": func(p *provenancePolicy) { p.predicate = "https://slsa.dev/provenance/v0.2" },
	} {
		p := releasePolicy(fixtureVersion)
		set(&p)
		if err := verifyBundle(tm, raw, p, digest); err == nil {
			t.Errorf("%s: verified with a mismatched policy", name)
		}
	}
}

func TestVerifyReleaseProvenanceAPI(t *testing.T) {
	useFixtureRoot(t)
	const token = "test-token"
	bundle := readFixture(t, "v0.1.5-linux-amd64.sigstore.json")
	compressed := readFixture(t, "v0.1.5-linux-amd64.sigstore.json.sn")
	var mode atomic.Value
	var srvURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/attestations/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("attestations auth %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/sha256:"+fixtureDigest) {
			http.NotFound(w, r)
			return
		}
		switch mode.Load().(string) {
		case "inline":
			fmt.Fprintf(w, `{"attestations":[{"bundle":%s,"bundle_url":"%s/never"}]}`, bundle, srvURL)
		case "url":
			fmt.Fprintf(w, `{"attestations":[{"bundle":null,"bundle_url":"%s/blob/b.json.sn?sig=secret"}]}`, srvURL)
		case "empty":
			_, _ = w.Write([]byte(`{"attestations":[]}`))
		case "404":
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/blob/b.json.sn", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("bundle_url got Authorization %q", got)
		}
		_, _ = w.Write(compressed)
	})
	mux.HandleFunc("/never", func(w http.ResponseWriter, r *http.Request) {
		t.Error("bundle_url fetched although an inline bundle was present")
	})
	srvURL = useAPI(t, mux).URL
	ctx := context.Background()
	digest := mustDigest(t, fixtureDigest)

	mode.Store("inline")
	if err := verifyReleaseProvenance(ctx, token, fixtureVersion, digest); err != nil {
		t.Fatalf("inline: %v", err)
	}
	if err := verifyReleaseProvenance(ctx, token, "v0.1.4", digest); err == nil {
		t.Fatal("inline: wrong tag verified")
	}
	mode.Store("url")
	if err := verifyReleaseProvenance(ctx, token, fixtureVersion, digest); err != nil {
		t.Fatalf("bundle_url: %v", err)
	}
	for _, m := range []string{"empty", "404"} {
		mode.Store(m)
		err := verifyReleaseProvenance(ctx, token, fixtureVersion, digest)
		if err == nil || !strings.Contains(err.Error(), "no build provenance attestation") {
			t.Fatalf("%s: %v", m, err)
		}
	}
	wrong := bytes.Clone(digest)
	wrong[31] ^= 1
	if err := verifyReleaseProvenance(ctx, token, fixtureVersion, wrong); err == nil {
		t.Fatal("unknown digest verified")
	}
}

func TestVerifyReleaseProvenanceTrustFailure(t *testing.T) {
	prev := trustedMaterial
	trustedMaterial = func(context.Context) (root.TrustedMaterial, error) { return nil, errors.New("tuf offline") }
	t.Cleanup(func() { trustedMaterial = prev })
	err := verifyReleaseProvenance(context.Background(), "", fixtureVersion, mustDigest(t, fixtureDigest))
	if err == nil || !strings.Contains(err.Error(), "tuf offline") {
		t.Fatalf("trust failure: %v", err)
	}
}

func TestSnappyDecode(t *testing.T) {
	got, err := snappyDecode(readFixture(t, "v0.1.5-linux-amd64.sigstore.json.sn"), maxBundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"`)) {
		t.Fatalf("decoded prefix %q", got[:60])
	}
	// "abcd" then copy(offset 4, length 8): overlapping back-reference.
	got, err = snappyDecode([]byte{12, 3 << 2, 'a', 'b', 'c', 'd', 1 | (8-4)<<2, 4}, 100)
	if err != nil || string(got) != "abcdabcdabcd" {
		t.Fatalf("copy: %q %v", got, err)
	}
	for name, in := range map[string][]byte{
		"too large":   {0xff, 0xff, 0xff, 0xff, 0x0f},
		"bad offset":  {4, 1, 0},
		"short":       {10, 3 << 2, 'a'},
		"length lies": {9, 3 << 2, 'a', 'b', 'c', 'd'},
	} {
		if _, err := snappyDecode(in, 100); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// YOHO_E2E=1: download v0.1.5 linux/amd64 from GitHub and verify it
// against the live TUF root and attestation API.
func TestDownloadLiveProvenance(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	path, err := Download(context.Background(), fixtureVersion, "linux/amd64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := fileSumMatches(path, fixtureDigest); !ok {
		t.Fatalf("digest mismatch: %v", err)
	}
}
