package swarm

import (
	"context"
	"testing"
)

func TestOperationTracksSwarmConvergence(t *testing.T) {
	before := map[string]svcStatus{"app_web": {UpdateStarted: "old"}, "app_db": {UpdateStarted: "old"}}
	after := map[string]svcStatus{"app_web": {UpdateStarted: "new"}, "app_db": {UpdateStarted: "old"}, "app_worker": {UpdateStarted: "new"}}
	got := map[string]string{}
	calls := 0
	r := &runner{onService: func(_ context.Context, name, state string) { got[name] = state; calls++ }}
	r.reportConvergence(context.Background(), "app", before, after, []string{"app_worker"}, nil)
	if got["web"] != "changed" || got["db"] != "unchanged" || got["worker"] != "attempted" {
		t.Fatal(got)
	}
	r.reportConvergence(context.Background(), "app", before, after, []string{"app_worker"}, nil)
	if calls != 3 {
		t.Fatal("unchanged status emitted again")
	}
	r.reportConvergence(context.Background(), "app", before, after, nil, nil)
	if got["worker"] != "changed" || calls != 4 {
		t.Fatal(got, calls)
	}
}
