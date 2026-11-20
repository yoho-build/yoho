package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

func validateNetwork(c *Config) []error {
	var errs []error
	add := func(s string) { errs = append(errs, fmt.Errorf("network: %s", s)) }
	if c.Network.Mesh != "" && c.Network.Mesh != "none" && c.Network.Mesh != "wireguard" {
		add("mesh must be none or wireguard")
	}
	if c.Network.Port < 0 || c.Network.Port > 65535 {
		add("port must be 1..65535")
	}
	if c.Network.Subnet != "" {
		p, err := netip.ParsePrefix(c.Network.Subnet)
		if err != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p != p.Masked() || p.Bits() > 30 {
			add("subnet must be a canonical private IPv4 CIDR with usable addresses")
		}
	}
	public := false
	for _, name := range sortedKeys(c.Servers) {
		s := c.Servers[name]
		if s.Endpoint != "none" {
			public = true
		}
		if s.Endpoint == "" || s.Endpoint == "none" {
			continue
		}
		host := s.Endpoint
		if strings.Contains(host, ":") {
			var port string
			var err error
			host, port, err = net.SplitHostPort(host)
			n, e := strconv.Atoi(port)
			if err != nil || e != nil || n < 1 || n > 65535 {
				add("invalid endpoint for Server " + name)
				continue
			}
		}
		if host == "" || strings.ContainsAny(host, " \t\r\n/#;[]") {
			add("invalid endpoint for Server " + name)
		}
	}
	if c.Network.Mesh == "wireguard" && !public {
		add("wireguard needs a Server with a public endpoint")
	}
	return errs
}
