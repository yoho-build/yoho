package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/secrets"
)

// DefaultCloudflareAPI is the Cloudflare API base URL. YOHO_CLOUDFLARE_API
// overrides it (tests only).
const DefaultCloudflareAPI = "https://api.cloudflare.com/client/v4"

// tunnelIDPattern bounds account tags and tunnel IDs before they go into a URL.
var tunnelIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ParseTunnelToken returns the account tag and tunnel ID of a remotely
// managed tunnel token (base64 JSON {"a","t","s"}). The tunnel secret is
// not returned.
func ParseTunnelToken(token string) (account, tunnel string, err error) {
	token = strings.TrimSpace(token)
	var raw []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(token); err == nil {
			break
		}
	}
	if err != nil {
		return "", "", errors.New("the tunnel token is not base64; copy it again from Cloudflare Zero Trust > Networks > Tunnels")
	}
	var t struct {
		A string `json:"a"`
		T string `json:"t"`
	}
	if json.Unmarshal(raw, &t) != nil || !tunnelIDPattern.MatchString(t.A) || !tunnelIDPattern.MatchString(t.T) {
		return "", "", errors.New("the tunnel token does not name an account and tunnel; copy it again from Cloudflare Zero Trust > Networks > Tunnels")
	}
	return t.A, t.T, nil
}

// TunnelAPI reads and writes the configuration of one remotely managed
// Cloudflare Tunnel, and nothing else (ADR 0012). It runs on the operator's
// machine. The API token travels only in the Authorization header; errors
// are redacted.
type TunnelAPI struct {
	BaseURL   string
	AccountID string
	TunnelID  string
	Client    *http.Client
	token     string
	redact    []string
}

// NewTunnelAPI returns a client for the tunnel named by tunnelToken,
// authenticated with apiToken.
func NewTunnelAPI(apiToken, tunnelToken string) (*TunnelAPI, error) {
	account, tunnel, err := ParseTunnelToken(tunnelToken)
	if err != nil {
		return nil, err
	}
	base := DefaultCloudflareAPI
	if v := strings.TrimSpace(os.Getenv("YOHO_CLOUDFLARE_API")); v != "" {
		base = v
	}
	return &TunnelAPI{
		BaseURL: strings.TrimRight(base, "/"), AccountID: account, TunnelID: tunnel,
		Client: &http.Client{Timeout: 30 * time.Second},
		token:  apiToken, redact: []string{apiToken, tunnelToken},
	}, nil
}

func (c *TunnelAPI) configURL() string {
	return c.BaseURL + "/accounts/" + url.PathEscape(c.AccountID) + "/cfd_tunnel/" + url.PathEscape(c.TunnelID) + "/configurations"
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

type cfTunnelConfig struct {
	Source string          `json:"source"`
	Config json.RawMessage `json:"config"`
}

// GetConfig returns the tunnel's configuration object (ingress and the
// rest), exactly as Cloudflare returns it.
func (c *TunnelAPI) GetConfig(ctx context.Context) (json.RawMessage, error) {
	res, err := c.do(ctx, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	var tc cfTunnelConfig
	if err := json.Unmarshal(res, &tc); err != nil {
		return nil, fmt.Errorf("Cloudflare API: unexpected tunnel configuration response: %w", err)
	}
	if tc.Source == "local" || len(tc.Config) == 0 || string(tc.Config) == "null" {
		return nil, errors.New("tunnel " + c.TunnelID + " is locally managed (ingress lives in cloudflared's config.yml); migrate it to a remotely managed tunnel or switch ingress by hand")
	}
	return tc.Config, nil
}

// PutConfig replaces the tunnel's configuration object.
func (c *TunnelAPI) PutConfig(ctx context.Context, cfg json.RawMessage) error {
	body, err := json.Marshal(map[string]json.RawMessage{"config": cfg})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, body)
	return err
}

func (c *TunnelAPI) do(ctx context.Context, method string, body []byte) (_ json.RawMessage, err error) {
	defer func() { err = secrets.RedactError(err, c.redact...) }()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.configURL(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cloudflare API %s tunnel configuration: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("Cloudflare API %s tunnel configuration: %w", method, err)
	}
	var env cfEnvelope
	jerr := json.Unmarshal(data, &env)
	if resp.StatusCode/100 == 2 && jerr == nil && env.Success {
		return env.Result, nil
	}
	var msgs []string
	for _, e := range env.Errors {
		msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	detail := strings.Join(msgs, "; ")
	if detail == "" {
		detail = strings.TrimSpace(string(data))
		if len(detail) > 300 {
			detail = detail[:300]
		}
	}
	err = fmt.Errorf("Cloudflare API %s tunnel configuration: HTTP %d: %s", method, resp.StatusCode, detail)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		err = fmt.Errorf("%w\nhint: the API token must be valid, unexpired and have Account > Cloudflare Tunnel > Edit for account %s", err, c.AccountID)
	}
	return nil, err
}
