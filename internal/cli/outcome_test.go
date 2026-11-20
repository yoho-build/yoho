package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestMutationExitCodes(t *testing.T) {
	connect := &remote.ConnectError{ExitError: &remote.ExitError{Code: 255}}
	for _, tt := range []struct {
		name    string
		err     error
		started bool
		code    int
	}{
		{"success", nil, false, 0}, {"generic", errors.New("failure"), false, 1},
		{"validation", &validationError{errors.New("config")}, false, 10},
		{"lock", &deploy.LockedError{}, false, 11}, {"connect", connect, false, 12},
		{"partial", &deploy.Failure{Services: []deploy.ServiceFailure{{Service: "web", ProxyRoute: "switched"}, {Service: "worker", Convergence: "not-attempted"}}}, true, 13},
		{"jobs", &jobsFailedError{err: errors.New("jobs")}, true, 13},
		{"unknown", connect, true, 14}, {"disconnect", &remote.ExitError{Code: 255}, true, 14},
		{"stale", stalePlan("config_digest changed"), false, 15},
		{"exec passthrough", &exitError{42, errors.New("exec")}, true, 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.err
			if err != nil {
				err = fmt.Errorf("wrapped: %w", err)
			}
			if got := mutationCode(err, tt.started); got != tt.code {
				t.Fatalf("got %d want %d", got, tt.code)
			}
		})
	}
}

func TestOperationResultUsesPersistedServiceProgress(t *testing.T) {
	r := &auditRun{mutated: true, op: &operation.Record{Services: map[string]string{"web": "changed", "worker": "attempted"}}}
	ctx := context.WithValue(context.Background(), auditRunKey{}, r)
	err := classifyOperation(ctx, &deploy.Failure{Services: []deploy.ServiceFailure{{Service: "yoho-shop-production_web", Convergence: "completed replicas=1/1", ProxyRoute: "not-attempted"}}})
	var result *mutationError
	if !errors.As(err, &result) || result.code != 13 {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.result.Changed, []string{"web"}) || !reflect.DeepEqual(result.result.Unchanged, []string{"worker"}) {
		t.Fatal(result.result)
	}
}
