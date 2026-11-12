package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRefreshedJSON(t *testing.T) {
	for _, refreshed := range []bool{false, true} {
		data, err := json.Marshal(Release{Refreshed: refreshed})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"refreshed"`) != refreshed {
			t.Fatal(string(data))
		}
		var got Release
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Refreshed != refreshed {
			t.Fatal(string(data))
		}
	}
}
