package webui

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/dash"
)

// TestTrafficTypesMatchWeb keeps web/src/types.ts in step with the json
// tags of the Traffic types (the API contract).
func TestTrafficTypesMatchWeb(t *testing.T) {
	src, err := os.ReadFile("../../web/src/types.ts")
	if err != nil {
		t.Skip("web sources not available:", err)
	}
	for name, v := range map[string]any{"Traffic": dash.Traffic{}, "RouteTraffic": dash.RouteTraffic{}, "Bucket": dash.Bucket{}} {
		m := regexp.MustCompile(`(?s)export interface ` + name + ` \{(.*?)\n\}`).FindSubmatch(src)
		if m == nil {
			t.Errorf("types.ts lacks interface %s", name)
			continue
		}
		fields := map[string]bool{}
		for _, l := range strings.Split(string(m[1]), "\n") {
			if f, _, ok := strings.Cut(strings.TrimSpace(l), ":"); ok {
				fields[strings.TrimSuffix(f, "?")] = true
			}
		}
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			if !fields[tag] {
				t.Errorf("types.ts %s lacks %q", name, tag)
			}
			delete(fields, tag)
		}
		for f := range fields {
			t.Errorf("types.ts %s has %q, which dash.%s does not send", name, f, name)
		}
	}
	if !strings.Contains(string(src), "traffic?: Traffic") {
		t.Error("Server.traffic missing in types.ts")
	}
}
