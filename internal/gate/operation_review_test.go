package gate

import (
	"context"
	"errors"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"strings"
	"testing"
)

func TestReviewGateOperationStartsAfterLock(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{true: "held", false: "free"}[held], func(t *testing.T) {
			setup(t, "")
			locked := false
			recorded := false
			ran := false
			h := &fakeHost{reply: func(script string) (string, error) {
				if strings.Contains(script, "echo acquired; else echo held") {
					if held {
						return "held", nil
					}
					locked = true
					return "acquired", nil
				}
				if strings.Contains(script, "operation record") {
					if !locked {
						t.Fatal("operation persisted before lock")
					}
					recorded = true
				}
				return "", nil
			}}
			d := &plan.Deploy{App: "shop", Destination: "production", OperationID: "review123", Servers: []plan.NamedHost{{Host: h}}}
			_, err := recordOperation(context.Background(), d, "deploy", func() (*release.Release, error) { ran = true; return nil, nil })
			if held {
				var lock *deploy.LockedError
				if !errors.As(err, &lock) || recorded || ran {
					t.Fatalf("lock refusal changed state: %v", err)
				}
			} else if err != nil || !recorded || !ran {
				t.Fatalf("operation missing: %v", err)
			}
		})
	}
}
