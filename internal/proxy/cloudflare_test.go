package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testTunnelToken(account, tunnel string) string {
	b, _ := json.Marshal(map[string]string{"a": account, "t": tunnel, "s": "c2VjcmV0LXNlY3JldA=="})
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseTunnelToken(t *testing.T) {
	a, tun, err := ParseTunnelToken(" " + testTunnelToken("acct", "5d1c0f5e-0000-4000-8000-00000000abcd") + "\n")
	if err != nil || a != "acct" || tun != "5d1c0f5e-0000-4000-8000-00000000abcd" {
		t.Fatalf("%q %q %v", a, tun, err)
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte(`{"a":"x"}`)), testTunnelToken("../x", "t")} {
		if _, _, err := ParseTunnelToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), "c2VjcmV0") {
			t.Error("tunnel secret in error")
		}
	}
}

func TestTunnelAPIErrorsAreRedacted(t *testing.T) {
	const apiToken = "very-secret-api-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token %s"}]}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer srv.Close()
	t.Setenv("YOHO_CLOUDFLARE_API", srv.URL)
	c, err := NewTunnelAPI(apiToken, testTunnelToken("acct", "tun"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetConfig(context.Background())
	if err == nil || strings.Contains(err.Error(), apiToken) || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "Cloudflare Tunnel > Edit") {
		t.Fatalf("err = %v", err)
	}
}

func TestTunnelAPILocallyManaged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":{"tunnel_id":"tun","config":null,"source":"local"}}`)
	}))
	defer srv.Close()
	t.Setenv("YOHO_CLOUDFLARE_API", srv.URL+"/")
	c, _ := NewTunnelAPI("tok", testTunnelToken("acct", "tun"))
	if _, err := c.GetConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "locally managed") {
		t.Fatalf("err = %v", err)
	}
}
