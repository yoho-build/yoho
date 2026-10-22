package config

import (
	"strings"
	"testing"
)

func TestValidateSubnet(t *testing.T) {
	c, err := Parse([]byte(`
app: a
servers: {s: {ssh: x}}
destinations:
  ok: {servers: [s], subnet: 10.200.3.0/24}
  host: {servers: [s], subnet: 10.200.3.5/24}
  junk: {servers: [s], subnet: nope}
  v6: {servers: [s], subnet: "fd00::/64"}
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range Validate(c) {
		all = append(all, e.Error())
	}
	j := strings.Join(all, "\n")
	for _, bad := range []string{"host", "junk", "v6"} {
		if !strings.Contains(j, "destinations."+bad+".subnet") {
			t.Errorf("%s not rejected:\n%s", bad, j)
		}
	}
	if strings.Contains(j, "destinations.ok.subnet") {
		t.Errorf("valid subnet rejected:\n%s", j)
	}
	b, err := Schema()
	if err != nil || !strings.Contains(string(b), `"subnet"`) {
		t.Fatal("schema lacks subnet")
	}
}
