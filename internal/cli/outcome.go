package cli

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/remote"
)

// mutationError preserves the cause while carrying the stable automation result.
type mutationError struct {
	err    error
	code   int
	result operation.Result
}

func (e *mutationError) Error() string { return e.err.Error() }
func (e *mutationError) Unwrap() error { return e.err }
func (e *mutationError) OutcomeFields() (string, []string, []string) {
	return e.result.Outcome, e.result.Changed, e.result.Unchanged
}
func mutationResult(err error, started bool) operation.Result {
	r := operation.Result{Outcome: "success"}
	if err == nil {
		return r
	}
	r.Outcome = "failed"
	var me *mutationError
	if errors.As(err, &me) {
		return me.result
	}
	var ge *remote.GateError
	if errors.As(err, &ge) && ge.Outcome != "" {
		return operation.Result{Outcome: ge.Outcome, Changed: ge.Changed, Unchanged: ge.Unchanged}
	}
	var jobs *jobsFailedError
	if errors.As(err, &jobs) {
		r.Outcome = "partial"
	}
	var rolesFailure *deploy.RolesFailure
	if errors.As(err, &rolesFailure) {
		r.Changed = slices.Clone(rolesFailure.Changed)
		r.Unchanged = slices.Clone(rolesFailure.Unchanged)
		if len(r.Changed) > 0 {
			r.Outcome = "partial"
		}
	}
	var failure *deploy.Failure
	if rolesFailure == nil && errors.As(err, &failure) {
		for _, s := range failure.Services {
			if s.ProxyRoute == "switched" || (s.ProxyRoute == "not-applicable" && (s.Convergence == "complete" || s.Convergence == "ready")) {
				r.Changed = append(r.Changed, s.Service)
			} else {
				r.Unchanged = append(r.Unchanged, s.Service)
			}
		}
		if len(r.Changed) > 0 {
			r.Outcome = "partial"
		}
	}
	var ee *remote.ExitError
	if started && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || remote.IsDisconnect(err) || errors.As(err, &ee) && (ee.Code == 255 || ee.Code == -1)) {
		r.Outcome = "unknown"
	}
	if started {
		if _, ok := remote.AsConnectError(err); ok {
			r.Outcome = "unknown"
		}
	}
	sort.Strings(r.Changed)
	sort.Strings(r.Unchanged)
	return r
}
func mutationCode(err error, started bool) int {
	if err == nil {
		return 0
	}
	var me *mutationError
	if errors.As(err, &me) {
		return me.code
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	switch mutationResult(err, started).Outcome {
	case "partial":
		return 13
	case "unknown":
		return 14
	}
	var ge *remote.GateError
	if errors.As(err, &ge) {
		switch ge.Code {
		case "locked":
			return 11
		case "invalid", "denied", "unsupported":
			return 10
		}
	}
	var validation *validationError
	if errors.As(err, &validation) {
		return 10
	}
	var locked *deploy.LockedError
	if errors.As(err, &locked) {
		return 11
	}
	if _, ok := remote.AsConnectError(err); ok {
		return 12
	}
	return 1
}
func classifyMutation(err error, started bool) error {
	if err == nil {
		return nil
	}
	return &mutationError{err, mutationCode(err, started), mutationResult(err, started)}
}
func operationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(operationKey{}).(string)
	return id
}

type operationKey struct{}

func preMutation(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	r, _ := ctx.Value(auditRunKey{}).(*auditRun)
	return r == nil || !r.mutated
}

func classifyOperation(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	r, _ := ctx.Value(auditRunKey{}).(*auditRun)
	started := r != nil && r.mutated
	result := mutationResult(err, started)
	if r != nil && r.op != nil && len(r.op.Services) > 0 && result.Outcome != "unknown" {
		var rolesFailure *deploy.RolesFailure
		if !errors.As(err, &rolesFailure) {
			result.Changed = []string{}
			result.Unchanged = []string{}
		}

		for name, state := range r.op.Services {
			if state == "changed" {
				result.Changed = append(result.Changed, name)
			} else {
				result.Unchanged = append(result.Unchanged, name)
			}
		}
		result.Unchanged = slices.DeleteFunc(result.Unchanged, func(s string) bool { return slices.Contains(result.Changed, s) })
		sort.Strings(result.Changed)
		result.Changed = slices.Compact(result.Changed)
		sort.Strings(result.Unchanged)
		result.Unchanged = slices.Compact(result.Unchanged)
		if len(result.Changed) > 0 {
			result.Outcome = "partial"
		}
	}
	code := mutationCode(err, started)
	if result.Outcome == "partial" {
		code = 13
	}
	return &mutationError{err, code, result}
}

type validationError struct{ err error }

func (e *validationError) Error() string { return e.err.Error() }
func (e *validationError) Unwrap() error { return e.err }

func markScopedMutation(ctx context.Context) {
	if r, _ := ctx.Value(auditRunKey{}).(*auditRun); r != nil {
		r.mutated = true
	}
}
