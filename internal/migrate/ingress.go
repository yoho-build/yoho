package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CloudflareAPI reads and writes one tunnel's configuration object.
type CloudflareAPI interface {
	GetConfig(ctx context.Context) (json.RawMessage, error)
	PutConfig(ctx context.Context, cfg json.RawMessage) error
}

// IngressRule is one rule of a tunnel configuration.
type IngressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Path     string `json:"path,omitempty"`
	Service  string `json:"service"`
}

func parseConfig(cfg json.RawMessage) (map[string]json.RawMessage, []map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &top); err != nil || top == nil {
		return nil, nil, fmt.Errorf("tunnel configuration is not a JSON object")
	}
	var rules []map[string]json.RawMessage
	if raw, ok := top["ingress"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &rules); err != nil {
			return nil, nil, fmt.Errorf("tunnel configuration ingress is not a list of rules")
		}
	}
	return top, rules, nil
}

func ruleString(r map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := r[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Rules lists the ingress rules of cfg in order.
func Rules(cfg json.RawMessage) ([]IngressRule, error) {
	_, rules, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]IngressRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, IngressRule{Hostname: ruleString(r, "hostname"), Path: ruleString(r, "path"), Service: ruleString(r, "service")})
	}
	return out, nil
}

func hostSet(hostnames []string) map[string]bool {
	m := map[string]bool{}
	for _, h := range hostnames {
		m[strings.ToLower(h)] = true
	}
	return m
}

// MissingHostnames returns the hostnames without an ingress rule in cfg.
func MissingHostnames(cfg json.RawMessage, hostnames []string) ([]string, error) {
	rules, err := Rules(cfg)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range rules {
		have[strings.ToLower(r.Hostname)] = true
	}
	var missing []string
	for _, h := range hostnames {
		if !have[strings.ToLower(h)] {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

// RewriteIngress sets service on every ingress rule whose hostname is in
// hostnames. Every other field and rule, including the final catch-all,
// is kept as it was.
func RewriteIngress(cfg json.RawMessage, hostnames []string, service string) (json.RawMessage, error) {
	top, rules, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	want := hostSet(hostnames)
	svc, _ := json.Marshal(service)
	for _, r := range rules {
		if h := ruleString(r, "hostname"); h != "" && want[strings.ToLower(h)] {
			r["service"] = svc
		}
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	top["ingress"] = raw
	return json.Marshal(top)
}

// Hash identifies a tunnel configuration independent of key order and
// whitespace.
func Hash(cfg json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.UseNumber()
	var v any
	canon := []byte(cfg)
	if dec.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			canon = b
		}
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:16])
}
