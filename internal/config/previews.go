package config

import (
	"fmt"
	"regexp"
	"strings"
)

var previewDashes = regexp.MustCompile(`[^a-z0-9]+`)

// PreviewName normalizes a branch or PR name into a Destination suffix.
// Forty characters leave 23 characters of a DNS label for pattern prefixes.
// Docker project and route names have no DNS-label limit; rendered hosts do.
func PreviewName(raw string) (string, error) {
	n := strings.Trim(previewDashes.ReplaceAllString(strings.ToLower(raw), "-"), "-")
	if len(n) > 40 {
		n = strings.TrimRight(n[:40], "-")
	}
	if n == "" {
		return "", fmt.Errorf("preview name must contain a letter or digit")
	}
	return n, nil
}

func (p PreviewsConfig) RenderHost(name, service string) (string, error) {
	h := strings.ReplaceAll(strings.ReplaceAll(p.Host, "{name}", name), "{service}", service)
	if !strings.Contains(p.Host, "{name}") || len(h) > 253 || !hostnamePattern.MatchString(h) {
		return "", fmt.Errorf("previews.host must contain {name} and render a valid DNS hostname (labels at most 63 characters): %q", h)
	}
	return strings.ToLower(h), nil
}

func ValidatePreviews(c *Config) []error {
	if c.Previews == nil {
		return nil
	}
	var errs []error
	d, ok := c.Destinations[c.Previews.From]
	if !ok {
		errs = append(errs, fmt.Errorf("previews.from: unknown template Destination %q", c.Previews.From))
	} else if d.Runtime == "swarm" || d.Access == "scoped" {
		errs = append(errs, fmt.Errorf("previews.from requires a compose Destination with operator access"))
	}
	if _, err := c.Previews.RenderHost("preview", "web"); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// PreviewDestination preserves Server connection settings and interpolation,
// but never the template's data directory, fixed subnet or migration cutover.
func PreviewDestination(d Destination) Destination {
	d.AppDir, d.Subnet = "", ""
	d.Migrate = nil
	if d.Proxy != nil {
		p := *d.Proxy
		p.Tunnel = nil
		d.Proxy = &p
	}
	return d
}
