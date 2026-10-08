package cli

import (
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestBuiltServicesExcludesPulledImages(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"web":  {Name: "web", WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "."}}},
		"api":  {Name: "api", WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "./api"}}},
		"shop": {Name: "shop", ContainerSpec: types.ContainerSpec{Image: "vendor/shop-web:v1"}},
	}}
	images := map[string]string{"web": "app-web:abc", "api": "app-api:abc", "shop": "vendor/shop-web:v1"}
	if got, want := builtServices(p, images), []string{"api", "web"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("built = %v, want %v", got, want)
	}
	if got := builtServices(&types.Project{Services: types.Services{"shop": {ContainerSpec: types.ContainerSpec{Image: "x"}}}}, map[string]string{"shop": "x"}); got == nil || len(got) != 0 {
		t.Fatalf("built = %#v, want empty non-nil", got)
	}
}
