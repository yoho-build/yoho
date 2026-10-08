package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

func TestAdvertiseAddr(t *testing.T) {
	if a, err := advertiseAddr("s", config.Server{SSH: "u@203.0.113.5", PrivateAddress: "100.64.0.2"}); err != nil || a != "100.64.0.2" {
		t.Errorf("private: %q %v", a, err)
	}
	if a, err := advertiseAddr("s", config.Server{SSH: "u@203.0.113.5:2222"}); err != nil || a != "203.0.113.5" {
		t.Errorf("ssh ip: %q %v", a, err)
	}
	if _, err := advertiseAddr("s", config.Server{SSH: "u@example.com"}); err == nil {
		t.Error("hostname must require private_address")
	}
}

func TestSwarmRuntimeError(t *testing.T) {
	if err := swarmRuntimeError("production", config.Destination{Runtime: "swarm"}); err != nil {
		t.Fatal(err)
	}
	want := "destination production uses runtime compose; set destinations.production.runtime: swarm first (switching needs downtime, see docs/site/swarm.md)"
	if err := swarmRuntimeError("production", config.Destination{}); err == nil || err.Error() != want {
		t.Fatalf("empty runtime: %v", err)
	}
	if err := swarmRuntimeError("edge", config.Destination{Runtime: "compose"}); err == nil || !strings.Contains(err.Error(), "destination edge uses runtime compose; set destinations.edge.runtime: swarm first") {
		t.Fatalf("compose runtime: %v", err)
	}
}

func TestSplitRows(t *testing.T) {
	got := splitRows("a|b|c\n\nd|e\n", 3)
	if !reflect.DeepEqual(got, [][]string{{"a", "b", "c"}, {"d", "e", ""}}) {
		t.Errorf("%v", got)
	}
}
