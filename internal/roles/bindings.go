package roles

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// CheckBindings verifies actual Docker publications, not just desired config.
func CheckBindings(ctx context.Context, h remote.Host, app, dest, server string, d config.Destination, addresses map[string]string) error {
	for _, c := range d.Connections {
		hs := d.ServiceServers(c.Service)
		if len(hs) != 1 || hs[0] != server {
			continue
		}
		ids, err := h.Output(ctx, remote.Cmd{Script: "set -eu\ndocker ps -q --filter " + remote.Quote("label=com.docker.compose.project="+release.ProjectName(app, dest)) + " --filter " + remote.Quote("label=com.docker.compose.service="+c.Service)})
		if err != nil {
			return err
		}
		if ids == "" {
			return fmt.Errorf("connection provider %s is not running", c.Service)
		}
		// IDs are obtained from Docker, but quote each separately.
		script := "set -eu\n"
		for _, id := range strings.Fields(ids) {
			script += "docker inspect --format '{{json .HostConfig.PortBindings}}' " + remote.Quote(id) + "\n"
		}
		out, err := h.Output(ctx, remote.Cmd{Script: script})
		if err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(out))
		for dec.More() {
			var bindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			}
			if err := dec.Decode(&bindings); err != nil {
				return err
			}
			bs := bindings[strconv.Itoa(c.Port)+"/tcp"]
			if len(bs) != 1 || bs[0].HostIP != addresses[server] || bs[0].HostPort != strconv.Itoa(c.Port) {
				return fmt.Errorf("connection %s:%d is not bound exclusively to %s", c.Service, c.Port, addresses[server])
			}
		}
	}
	return nil
}
