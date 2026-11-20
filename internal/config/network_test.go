package config

import (
	"strings"
	"testing"
)

func TestNetworkDefaultsAndValidation(t *testing.T) {
	c, e := Parse([]byte("app: demo\nservers: {a: {ssh: root@203.0.113.1}}\ndestinations: {production: {servers: [a]}}\n"), "test")
	if e != nil {
		t.Fatal(e)
	}
	if c.Network.Mesh != "none" || c.Network.Port != 51820 || c.Network.Subnet != "10.210.0.0/24" {
		t.Fatal(c.Network)
	}
	for _, tc := range []struct {
		n        NetworkConfig
		endpoint string
		bad      bool
	}{{NetworkConfig{Mesh: "wireguard", Subnet: "10.210.0.0/24", Port: 51820}, "", false}, {NetworkConfig{Mesh: "other"}, "", true}, {NetworkConfig{Subnet: "192.0.2.0/24"}, "", true}, {NetworkConfig{Subnet: "10.210.0.1/24"}, "", true}, {NetworkConfig{Port: 65536}, "", true}, {NetworkConfig{Mesh: "wireguard"}, "none", true}, {NetworkConfig{}, "host:invalid", true}, {NetworkConfig{}, "host\nPostUp=bad", true}, {NetworkConfig{}, "example.com:51821", false}} {
		c.Network = tc.n
		s := c.Servers["a"]
		s.Endpoint = tc.endpoint
		c.Servers["a"] = s
		if es := validateNetwork(c); (len(es) > 0) != tc.bad {
			t.Fatalf("%+v: %v", tc, es)
		}
	}
	s := Server{PrivateAddress: "10.0.0.1"}
	if s.EffectivePrivateAddress("") != "10.0.0.1" || s.EffectivePrivateAddress("10.210.0.1") != "10.210.0.1" {
		t.Fatal("effective address")
	}
	b, e := Schema()
	if e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{`"network"`, `"endpoint"`, `"wireguard"`} {
		if !strings.Contains(string(b), field) {
			t.Fatal("schema missing", field)
		}
	}
}

func TestNetworkPrefixContainment(t *testing.T) {
	for _, raw := range []string{"10.0.0.0/7", "192.168.0.0/15", "172.16.0.0/11"} {
		if errs := validateNetwork(&Config{Network: NetworkConfig{Subnet: raw}}); len(errs) == 0 {
			t.Fatal("accepted", raw)
		}
	}
	for _, raw := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if errs := validateNetwork(&Config{Network: NetworkConfig{Subnet: raw}}); len(errs) != 0 {
			t.Fatal(raw, errs)
		}
	}
	for _, subnet := range []string{"10.210.0.0/24", "10.210.0.0/25", "10.0.0.0/8"} {
		c := &Config{Network: NetworkConfig{Mesh: "wireguard"}, Servers: map[string]Server{"a": {SSH: "root@203.0.113.1"}}, Destinations: map[string]Destination{"production": {Subnet: subnet}}}
		if errs := validateNetwork(c); len(errs) == 0 {
			t.Fatal("overlap accepted", subnet)
		}
	}
	c := &Config{Network: NetworkConfig{Mesh: "wireguard", Subnet: "10.210.0.0/30"}, Servers: map[string]Server{"a": {}, "b": {}, "c": {}}}
	if errs := validateNetwork(c); len(errs) == 0 {
		t.Fatal("capacity accepted")
	}
}
