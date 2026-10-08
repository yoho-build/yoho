package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
)

func TestHintForOwnership(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &release.OwnershipError{Msg: "collision"})
	if got := hintFor(err, "check that Docker is running"); !strings.Contains(got, "rename the App or Destination") {
		t.Errorf("hint %q", got)
	}
	if got := hintFor(fmt.Errorf("boom"), "check that Docker is running"); got != "check that Docker is running" {
		t.Errorf("hint %q", got)
	}
}
