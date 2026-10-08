package deploy

import (
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

func durPtr(d time.Duration) *types.Duration {
	v := types.Duration(d)
	return &v
}

func TestSupportsStartInterval(t *testing.T) {
	ok := []string{"25", "25.0.0", "25.0.3", "29.1.0", "25.0.0-rc.1"}
	for _, v := range ok {
		if !SupportsStartInterval(v) {
			t.Errorf("%q should support start_interval", v)
		}
	}
	bad := []string{"", "24", "24.0.9", "v25.0.0", "nope", "  "}
	for _, v := range bad {
		if SupportsStartInterval(v) {
			t.Errorf("%q should not support start_interval", v)
		}
	}
}

func TestUseFastStart(t *testing.T) {
	hc := &types.HealthCheckConfig{Test: []string{"CMD", "true"}}
	proxied := config.ServiceExt{Proxy: &config.ServiceProxy{}}
	plain := config.ServiceExt{}

	if !UseFastStart(proxied, hc, false, "25.0.1") {
		t.Error("proxied healthcheck on Docker 25")
	}
	if UseFastStart(plain, hc, false, "25.0.1") {
		t.Error("compose does not wait on a non-proxied healthcheck")
	}
	if !UseFastStart(plain, hc, true, "29.0.0") {
		t.Error("swarm waits on every healthcheck")
	}
	if UseFastStart(proxied, hc, false, "24.0.7") || UseFastStart(proxied, hc, false, "") {
		t.Error("Docker before 25 must not get start_interval")
	}
	if UseFastStart(proxied, nil, false, "25.0.0") {
		t.Error("no healthcheck")
	}
	disabled := &types.HealthCheckConfig{Disable: true}
	if UseFastStart(proxied, disabled, true, "25.0.0") {
		t.Error("disabled healthcheck")
	}
	withPeriod := &types.HealthCheckConfig{Test: hc.Test, StartPeriod: durPtr(10 * time.Second)}
	if UseFastStart(proxied, withPeriod, false, "25.0.0") {
		t.Error("user start_period must win")
	}
	withInterval := &types.HealthCheckConfig{Test: hc.Test, StartInterval: durPtr(time.Second)}
	if UseFastStart(proxied, withInterval, true, "25.0.0") {
		t.Error("user start_interval must win")
	}
}
