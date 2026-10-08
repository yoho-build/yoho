package cli

import (
	"reflect"
	"testing"

	"github.com/yoho-dev/yoho/internal/config"
)

func TestAdvertiseAddr(t *testing.T) {
	if a, err := advertiseAddr("s", config.Server{SSH: "u@203.0.113.5", PrivateAddress: "100.64.0.2"}); err != nil || a != "100.64.0.2" {
		t.Errorf("private: %q %v", a, err)
	}
	if a, err := advertiseAddr("s", config.Server{SSH: "u@203.0.113.5:2222"}); err != nil || a != "203.0.113.5" {
		t.Errorf("ssh ip: %q %v", a, err)
	}
	if _, err := advertiseAddr("s", config.Server{SSH: "u@example.com"}); err == nil {
		t.Error("hostname must require private_address")
	}
}

func TestSplitRows(t *testing.T) {
	got := splitRows("a|b|c\n\nd|e\n", 3)
	if !reflect.DeepEqual(got, [][]string{{"a", "b", "c"}, {"d", "e", ""}}) {
		t.Errorf("%v", got)
	}
}
