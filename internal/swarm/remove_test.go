package swarm

import (
	"strings"
	"testing"
)

func TestStackRmScriptExactNames(t *testing.T) {
	s := StackRmScript("shop", "production")
	if !strings.Contains(s, "for s in 'yoho-shop-production' 'yoho-shop-production-release'; do") ||
		!strings.Contains(s, `grep -qxF -- "$s"`) || !strings.Contains(s, `docker stack rm "$s"`) {
		t.Fatalf("script:\n%s", s)
	}
	if strings.Contains(s, "-v") && !strings.Contains(s, "grep -qxF") {
		t.Fatalf("volumes in stack rm:\n%s", s)
	}
}
