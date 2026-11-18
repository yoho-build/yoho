package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestServiceCheck(t *testing.T) {
	e, err := DecodeServiceExt(map[string]any{"check": map[string]any{"type": "websocket", "port": 8080, "path": "/sync"}})
	if err != nil {
		t.Fatal(err)
	}
	c := *e.Check
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.WithDefaults(); got != (ServiceCheck{Type: "websocket", Port: 8080, Path: "/sync", Host: "localhost", Timeout: "30s"}) {
		t.Fatalf("defaults: %+v", got)
	}
	if got := (ServiceCheck{Type: "tcp", Port: 1, Timeout: "2m"}).WithDefaults(); got != (ServiceCheck{Type: "tcp", Port: 1, Timeout: "2m0s"}) {
		t.Fatalf("tcp defaults: %+v", got)
	}
	if d := (ServiceCheck{Type: "tcp", Port: 1, Timeout: "90s"}).TimeoutDuration(); d != 90*time.Second {
		t.Fatalf("timeout %s", d)
	}
	if s := c.String(); s != "websocket /sync on port 8080" {
		t.Fatalf("String: %q", s)
	}
	for _, bad := range []ServiceCheck{
		{Type: "tcp", Port: 1, Timeout: "0s"},
		{Type: "tcp", Port: 1, Timeout: "soon"},
		{Type: "tcp", Port: 1, Timeout: "10m1s"},
		{Type: "http", Port: 1, Host: "a b"},
		{Type: "http", Port: 1, Path: "http://evil/"},
		{Type: "tcp", Port: 1, Host: "x"},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := DecodeServiceExt(map[string]any{"check": map[string]any{"type": "tcp", "port": 1, "target": "10.0.0.1"}}); err == nil || !strings.Contains(err.Error(), `unknown key "check.target"`) {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestServiceCheckSchema(t *testing.T) {
	b, err := ServiceExtSchema()
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	d, ok := s.Defs["ServiceCheck"]
	if !ok {
		t.Fatalf("no ServiceCheck in %s", b)
	}
	if strings.Join(d.Required, ",") != "type,port" || d.Properties["type"]["enum"] == nil || d.Properties["timeout"]["description"] == nil {
		t.Fatalf("ServiceCheck schema: %+v", d)
	}
}
