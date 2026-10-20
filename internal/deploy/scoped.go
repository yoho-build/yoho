package deploy

import (
	"context"
	"fmt"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/remote"
)

// LabelProxyNetwork marks a scoped App's own network to the Proxy. The
// Proxy is (re)connected to every network with this label, see
// proxy.BootWith.
const LabelProxyNetwork = proxy.LabelAttach

// proxyNetworkName is the Docker network proxied Services join: the shared
// `yoho` network, or a scoped App's own network.
func proxyNetworkName(d *plan.Deploy) string {
	if d.Scoped != nil && d.Scoped.ProxyNetwork != "" {
		return d.Scoped.ProxyNetwork
	}
	return proxy.Network
}

// ensureScopedNetwork creates a scoped App's own Proxy network (labelled
// with the App Destination) and connects the Proxy to it. Idempotent; a
// network of that name that is not the App's is refused.
func ensureScopedNetwork(ctx context.Context, h remote.Host, d *plan.Deploy) error {
	if d.Scoped == nil || d.Scoped.ProxyNetwork == "" {
		return nil
	}
	n := remote.Quote(d.Scoped.ProxyNetwork)
	owner := d.App + "|" + d.Destination
	script := "set -eu\n" +
		"if ! docker network inspect " + n + " >/dev/null 2>&1; then\n" +
		"  docker network create --label " + remote.Quote(LabelProxyNetwork+"=1") +
		" --label " + remote.Quote(LabelApp+"="+d.App) + " --label " + remote.Quote(LabelDestination+"="+d.Destination) + " " + n + " >/dev/null\n" +
		"fi\n" +
		"own=$(docker network inspect -f '{{index .Labels \"" + LabelApp + "\"}}|{{index .Labels \"" + LabelDestination + "\"}}|{{index .Labels \"" + LabelProxyNetwork + "\"}}' " + n + ")\n" +
		"if [ \"$own\" != " + remote.Quote(owner+"|1") + " ]; then echo 'docker network '" + n + "' exists but is not the Proxy network of " + d.App + "/" + d.Destination + "' >&2; exit 1; fi\n" +
		"if docker container inspect " + proxy.ContainerName + " >/dev/null 2>&1; then\n" +
		"  case \" $(docker container inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' " + proxy.ContainerName + ") \" in\n" +
		"  *\" \"" + n + "\" \"*) ;;\n" +
		"  *) docker network connect " + n + " " + proxy.ContainerName + " ;;\n" +
		"  esac\n" +
		"fi\n"
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("connect the Proxy to %s: %w", d.Scoped.ProxyNetwork, err)
	}
	return nil
}

// lockInfo adds the Gate process to a lock record.
func lockInfo(d *plan.Deploy, info LockInfo) LockInfo {
	if d.Scoped != nil {
		info.PID, info.Boot = d.Scoped.PID, d.Scoped.Boot
	}
	return info
}
