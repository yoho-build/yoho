package composefile

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

// CheckCloudflareSSH warns when dest.ssh.via is cloudflare and a
// cloudflared Service in this App would drop that SSH session when a
// deploy restarts it.
func CheckCloudflareSSH(destName string, dest config.Destination, p *types.Project) []Finding {
	if dest.SSH == nil || dest.SSH.Via != "cloudflare" || dest.SSH.Hostname == "" || p == nil {
		return nil
	}
	var names []string
	for _, name := range p.ServiceNames() {
		if cloudflaredService(name, p.Services[name]) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	who := "Service " + names[0]
	if len(names) > 1 {
		who = "Services " + strings.Join(names, ", ")
	}
	return []Finding{{
		Level:   LevelWarning,
		Service: names[0],
		Message: fmt.Sprintf("destinations.%s.ssh hostname %s is reached through Cloudflare; %s in this App is a cloudflared connector, so a deploy that restarts it drops the SSH session (exit 255). Gate rollouts finish on the Server; yoho deploy reconnects, or yoho releases shows the outcome. Prefer a dedicated SSH tunnel outside this App", destName, dest.SSH.Hostname, who),
	}}
}

func cloudflaredService(name string, svc types.ServiceConfig) bool {
	if strings.EqualFold(name, "cloudflared") {
		return true
	}
	return strings.Contains(strings.ToLower(svc.Image), "cloudflare/cloudflared")
}
