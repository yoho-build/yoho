package deploy

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// Fast-start healthcheck defaults (Docker Engine >= 25). Without
// start_interval the first probe waits a full interval. During start_period,
// probes run every second and failures do not count, so a slow boot is not
// marked unhealthy.
const (
	FastStartPeriod   = "60s"
	FastStartInterval = "1s"
)

// DockerServerVersion is the engine version from `docker version`, or ""
// when the probe fails. Callers treat "" as "do not emit start_interval":
// Docker before 25 rejects that field.
func DockerServerVersion(ctx context.Context, h remote.Host) string {
	if h == nil {
		return ""
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "docker version --format '{{.Server.Version}}'"})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// SupportsStartInterval reports whether version is Docker Engine 25 or
// newer, when healthcheck.start_interval was added. Empty or unparseable
// versions report false.
func SupportsStartInterval(version string) bool {
	major, ok := dockerMajor(version)
	return ok && major >= 25
}

func dockerMajor(version string) (int, bool) {
	version = strings.TrimSpace(version)
	i := 0
	for i < len(version) && version[i] >= '0' && version[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(version[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// HealthGated reports whether Yoho waits on hc before treating a new
// container or task as ready. Proxied services are waited on in both
// runtimes. waitOnAll is swarm stack deploy, which waits for every service
// that defines a healthcheck.
func HealthGated(ext config.ServiceExt, hc *types.HealthCheckConfig, waitOnAll bool) bool {
	if hc == nil || hc.Disable {
		return false
	}
	if ext.Proxy != nil {
		return true
	}
	return waitOnAll
}

// UseFastStart reports whether the compiled healthcheck should gain
// start_period 60s and start_interval 1s. A user-set start_period or
// start_interval is left unchanged.
func UseFastStart(ext config.ServiceExt, hc *types.HealthCheckConfig, waitOnAll bool, dockerVersion string) bool {
	if !SupportsStartInterval(dockerVersion) || !HealthGated(ext, hc, waitOnAll) {
		return false
	}
	return hc.StartPeriod == nil && hc.StartInterval == nil
}

// injectFastStart writes the fast-start fields into services that do not
// already set them. names come from UseFastStart; the YAML is checked
// again so a user value cannot be overwritten.
func injectFastStart(src []byte, names []string) ([]byte, error) {
	if len(names) == 0 {
		return src, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("re-parse compiled compose: %w", err)
	}
	if len(doc.Content) == 0 {
		return src, nil
	}
	svcs := mappingChild(doc.Content[0], "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return src, nil
	}
	for i := 0; i+1 < len(svcs.Content); i += 2 {
		if !want[svcs.Content[i].Value] {
			continue
		}
		hc := mappingChild(svcs.Content[i+1], "healthcheck")
		if hc == nil || hc.Kind != yaml.MappingNode {
			continue
		}
		if mappingChild(hc, "start_period") != nil || mappingChild(hc, "start_interval") != nil {
			continue
		}
		if n := mappingChild(hc, "disable"); n != nil && (n.Value == "true" || n.Value == "True") {
			continue
		}
		hc.Content = append(hc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "start_period"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: FastStartPeriod},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "start_interval"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: FastStartInterval},
		)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mappingChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
