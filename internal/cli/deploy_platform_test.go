package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

type platformHost struct {
	tunnelHost
	platform string
	err      error
}

func (h *platformHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	return h.platform, h.err
}

func TestDeploymentPlatform(t *testing.T) {
	for _, tc := range []struct {
		name      string
		servers   []string
		platforms []string
		want      string
		wantErr   string
	}{
		{name: "detect amd64", servers: []string{"linux/x86_64"}, want: "linux/amd64"},
		{name: "detect arm64", servers: []string{"linux/aarch64"}, want: "linux/arm64"},
		{name: "explicit amd64", servers: []string{"linux/amd64"}, platforms: []string{"linux/amd64"}, want: "linux/amd64"},
		{name: "arm64 listed first", servers: []string{"linux/amd64"}, platforms: []string{"linux/arm64", "linux/amd64"}, want: "linux/amd64"},
		{name: "amd64 listed first", servers: []string{"linux/arm64"}, platforms: []string{"linux/amd64", "linux/arm64"}, want: "linux/arm64"},
		{name: "arm64 variant", servers: []string{"linux/arm64"}, platforms: []string{"linux/amd64", "linux/arm64/v8"}, want: "linux/arm64/v8"},
		{name: "missing Server platform", servers: []string{"linux/amd64"}, platforms: []string{"linux/arm64"}, wantErr: "does not include Server platform linux/amd64"},
		{name: "same architecture", servers: []string{"linux/amd64", "linux/x86_64"}, want: "linux/amd64"},
		{name: "mixed architectures", servers: []string{"linux/amd64", "linux/arm64"}, platforms: []string{"linux/amd64", "linux/arm64"}, wantErr: "Servers have different platforms"},
		{name: "no Servers", wantErr: "without a Server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hosts []plan.NamedHost
			for _, p := range tc.servers {
				hosts = append(hosts, plan.NamedHost{Name: p, Host: &platformHost{platform: p}})
			}
			got, err := deploymentPlatform(context.Background(), hosts, tc.platforms)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %q, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			for _, h := range hosts {
				if len(h.Host.(*platformHost).scripts) != 1 {
					t.Fatal("each Server's platform must be detected")
				}
			}
		})
	}
}

func TestDeploymentPlatformDetectionFailure(t *testing.T) {
	broken := errors.New("Server unavailable")
	for _, failAt := range []int{0, 1} {
		hosts := []plan.NamedHost{
			{Name: "s1", Host: &platformHost{platform: "linux/amd64"}},
			{Name: "s2", Host: &platformHost{platform: "linux/amd64"}},
		}
		hosts[failAt].Host.(*platformHost).err = broken
		if _, err := deploymentPlatform(context.Background(), hosts, []string{"linux/amd64"}); !errors.Is(err, broken) {
			t.Fatalf("detection failure must not be hidden by explicit platforms: %v", err)
		}
	}
}

func TestBuildAndShipRejectsWrongPlatformBeforeBuild(t *testing.T) {
	a := &app{cfg: &config.Config{Builder: config.Builder{Platforms: []string{"linux/arm64"}}}, ui: ui.Discard()}
	s := &session{hosts: []plan.NamedHost{{Name: "s1", Host: &platformHost{platform: "linux/amd64"}}}}
	// No Hook or compose project: validation must fail before invoking either.
	if _, err := a.buildAndShip(context.Background(), s, false); err == nil || !strings.Contains(err.Error(), "does not include Server platform linux/amd64") {
		t.Fatalf("got %v", err)
	}
}
