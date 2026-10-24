package dist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestUsesGitHubLatestEndpoint(t *testing.T) {
	for _, token := range []string{"", "test-token"} {
		name := "without-token"
		if token != "" {
			name = "with-token"
		}
		t.Run(name, func(t *testing.T) {
			useToken(t, token)
			var requests int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/repos/"+Repo+"/releases/latest" {
					t.Errorf("request = %s %s, want GET /repos/%s/releases/latest", r.Method, r.URL.Path, Repo)
				}
				if got := r.Header.Get("Accept"); got != acceptAPI {
					t.Errorf("Accept = %q, want %q", got, acceptAPI)
				}
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if got := r.Header.Get("Authorization"); got != wantAuth {
					t.Errorf("Authorization = %q, want %q", got, wantAuth)
				}
				_, _ = fmt.Fprint(w, `{"tag_name":"v1.2.3","draft":false,"prerelease":false}`)
			}))
			defer srv.Close()
			useLatestAPIBase(t, srv.URL)

			got, err := Latest(context.Background())
			if err != nil || got != "v1.2.3" {
				t.Fatalf("Latest() = %q, %v; want v1.2.3", got, err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want only the latest release request", requests)
			}
		})
	}
}

func TestLatestRejectsMalformedAndUnstableResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"tag_name":`},
		{name: "missing tag", body: `{"draft":false,"prerelease":false}`},
		{name: "missing flags", body: `{"tag_name":"v1.2.3"}`},
		{name: "draft", body: `{"tag_name":"v1.2.3","draft":true,"prerelease":false}`},
		{name: "prerelease", body: `{"tag_name":"v1.2.3","draft":false,"prerelease":true}`},
		{name: "missing v prefix", body: `{"tag_name":"1.2.3","draft":false,"prerelease":false}`},
		{name: "leading zero", body: `{"tag_name":"v1.02.3","draft":false,"prerelease":false}`},
		{name: "prerelease tag", body: `{"tag_name":"v1.2.3-rc.1","draft":false,"prerelease":false}`},
		{name: "build metadata", body: `{"tag_name":"v1.2.3+build.1","draft":false,"prerelease":false}`},
		{name: "unsafe path tag", body: `{"tag_name":"v1.2.3/../../other","draft":false,"prerelease":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useToken(t, "")
			useLatestAPI(t, http.StatusOK, tc.body)
			if got, err := Latest(context.Background()); err == nil {
				t.Fatalf("Latest() = %q, want an error", got)
			}
		})
	}
}

func TestLatestRejectsHTTPFailureAndOversizedResponse(t *testing.T) {
	t.Run("HTTP failure", func(t *testing.T) {
		useToken(t, "")
		useLatestAPI(t, http.StatusServiceUnavailable, `unavailable`)
		if got, err := Latest(context.Background()); err == nil || got != "" {
			t.Fatalf("Latest() = %q, %v; want an HTTP error", got, err)
		}
	})

	t.Run("response exceeds one MiB", func(t *testing.T) {
		useToken(t, "")
		body := `{"tag_name":"v1.2.3","draft":false,"prerelease":false,"extra":"` + strings.Repeat("x", maxLatestResponse) + `"}`
		useLatestAPI(t, http.StatusOK, body)
		if got, err := Latest(context.Background()); err == nil || got != "" {
			t.Fatalf("Latest() = %q, %v; want a size-limit error", got, err)
		}
	})
}

func TestLatestHonorsCanceledContext(t *testing.T) {
	useToken(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"tag_name":"v1.2.3","draft":false,"prerelease":false}`)
	}))
	defer srv.Close()
	useLatestAPIBase(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := Latest(ctx); got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("Latest(canceled) = %q, %v; want context.Canceled", got, err)
	}
}

func useLatestAPI(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	useLatestAPIBase(t, srv.URL)
}

func useLatestAPIBase(t *testing.T, base string) {
	t.Helper()
	prev := APIBase
	APIBase = base
	t.Cleanup(func() { APIBase = prev })
}
