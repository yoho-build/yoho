package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScheduledRetryConfigBoundsAndSchema(t *testing.T) {
	for _, tc := range []struct {
		retries, delay int
		schedule       string
		valid          bool
	}{{0, 0, "", true}, {5, 300, "weekly", true}, {-1, 0, "daily", false}, {6, 0, "daily", false}, {1, -1, "daily", false}, {1, 301, "daily", false}, {1, 0, "", false}} {
		c := Config{App: "shop", Servers: map[string]Server{"s": {SSH: "test"}}, Destinations: map[string]Destination{"prod": {Servers: []string{"s"}}}, Backups: BackupsConfig{Targets: map[string]BackupTarget{"t": {Repository: "/repo"}}, Jobs: map[string]BackupJob{"j": {Destination: "prod", Target: "t", Schedule: tc.schedule, Retries: tc.retries, RetryDelaySec: tc.delay}}}}
		if errs := Validate(&c); (len(errs) == 0) != tc.valid {
			t.Fatalf("%+v errs %v", tc, errs)
		}
	}
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	// Struct tags are the published schema source, not a hand-maintained file.
	s := string(b)
	for _, field := range []string{`"retries"`, `"retry_delay_sec"`, `"maximum": 5`, `"maximum": 300`, `"minimum": 0`} {
		if !strings.Contains(s, field) {
			t.Errorf("schema missing %s", field)
		}
	}
}
