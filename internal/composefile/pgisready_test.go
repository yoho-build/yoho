package composefile

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestSocketOnlyPgIsready(t *testing.T) {
	for cmd, want := range map[string]bool{
		"pg_isready -U postgres":                  true,
		"pg_isready -d postgres -U postgres":      true,
		"pg_isready -h 127.0.0.1 -U postgres":     false,
		"pg_isready -h127.0.0.1":                  false,
		"pg_isready --host=localhost":             false,
		"pg_isready -d 'host=127.0.0.1 dbname=x'": false,
		"wget -qO- http://127.0.0.1:8080/up":      false,
	} {
		hc := &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD-SHELL", cmd}}
		if got := socketOnlyPgIsready(hc); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
	if socketOnlyPgIsready(nil) || socketOnlyPgIsready(&types.HealthCheckConfig{Disable: true, Test: types.HealthCheckTest{"CMD-SHELL", "pg_isready"}}) {
		t.Error("no or disabled healthcheck is not flagged")
	}
}
