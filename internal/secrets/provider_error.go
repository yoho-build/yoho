package secrets

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// ErrClass says why a secrets provider call failed.
type ErrClass int

const (
	// Other is any failure Yoho cannot place; its text is the redacted
	// command error, as before.
	Other ErrClass = iota
	// ApprovalTimeout: the provider's approval prompt timed out or was denied.
	ApprovalTimeout
	// NotSignedIn: the provider has no session (or no token).
	NotSignedIn
	// NotFound: the item or field does not exist.
	NotFound
)

// approvalExitCode is EX_TEMPFAIL. A `command` provider that exits with it
// tells Yoho its own approval timed out, so the call is worth retrying.
const approvalExitCode = 75

// approvalElapsed is when a failing op / bw call with no recognised message
// is taken as an unanswered approval prompt: their own prompt timeout is
// about 60 s.
const approvalElapsed = 55 * time.Second

// ProviderError is a classified secrets provider failure. Its text is fixed
// per class and carries neither stderr nor stdout, so it is safe where
// command output is suppressed (the secrets.nonempty contract). Other keeps
// the redacted command error, which callers must not show under a contract.
type ProviderError struct {
	Class    ErrClass
	Provider string
	Elapsed  time.Duration
	// Inferred: ApprovalTimeout was concluded from the long wait alone; the
	// provider did not say so.
	Inferred bool
	// note is fixed text chosen by Yoho, never provider output.
	note   string
	detail error
}

func (e *ProviderError) Error() string {
	switch e.Class {
	case ApprovalTimeout:
		s := "secrets provider " + e.Provider + ": approval timed out or was not granted"
		if e.Inferred {
			s = "secrets provider " + e.Provider + ": failed with no reason Yoho recognises after a long wait, likely an unanswered approval prompt"
		}
		if e.Elapsed >= time.Second {
			s += " (waited " + e.Elapsed.Round(time.Second).String() + ")"
		}
		return s
	case NotSignedIn:
		s := "secrets provider " + e.Provider + ": not signed in"
		if e.note != "" {
			s += " (" + e.note + ")"
		}
		return s
	case NotFound:
		return "secrets provider " + e.Provider + ": item not found"
	}
	if e.detail != nil {
		return e.detail.Error()
	}
	return "secrets provider " + e.Provider + " failed"
}

// ErrorCode is the --json error code, empty for Other.
func (e *ProviderError) ErrorCode() string {
	switch e.Class {
	case ApprovalTimeout:
		return "secrets_approval_timeout"
	case NotSignedIn:
		return "secrets_not_signed_in"
	case NotFound:
		return "secrets_item_not_found"
	}
	return ""
}

// WaitEvent reports a provider call that has run longer than expected, most
// likely waiting for an approval. Key is the secret's name; the provider
// reference is deliberately absent because refs can hold credentials.
type WaitEvent struct {
	Provider string
	Key      string
	Elapsed  time.Duration
}

// watchWait calls onWait once if the returned stop is not called within
// after.
func watchWait(onWait func(WaitEvent), after time.Duration, ev WaitEvent) (stop func()) {
	if onWait == nil {
		return func() {}
	}
	start := time.Now()
	t := time.AfterFunc(after, func() {
		ev.Elapsed = time.Since(start)
		onWait(ev)
	})
	return func() { t.Stop() }
}

// providerPhrases are the provider's own message shapes. Generic text such as
// "not authorized" (a permission error) or "timed out" (a network error) is
// deliberately absent from approval: it must not be relabelled as an
// approval timeout or retried.
var providerPhrases = map[string]struct{ approval, signedIn, notFound []string }{
	"op": {
		approval: []string{"authorization timeout", "authorization prompt", "authorization denied", "authorization was denied"},
		signedIn: []string{"not signed in", "no accounts configured", "not currently signed in", "session expired"},
		notFound: []string{"isn't an item", "isn't a vault", "could not be found", "isn't a field"},
	},
	"bw": {
		signedIn: []string{"not logged in", "vault is locked", "invalid master password"},
		notFound: []string{"not found."},
	},
	"bws": {
		signedIn: []string{"[401 unauthorized]", "access token is not in a valid format", "--access-token <access_token>", `"invalid_client"`},
		notFound: []string{"[404 not found]", "resource not found"},
	},
}

// otherFailures are permission and network errors. They stay Other even
// after a long wait, so the wait heuristic below never turns them into an
// approval timeout.
var otherFailures = []string{
	"not authorized", "permission", "forbidden",
	"timed out", "timeout", "connection refused", "connection reset", "no such host", "network is unreachable", "dial tcp",
}

// interactive providers show an approval prompt of their own; only they get
// the elapsed-time inference.
var interactiveProvider = map[string]bool{"op": true, "bw": true}

// classify places a provider failure. stderr is read here and dropped.
// inferred is true when ApprovalTimeout rests on the wait alone.
func classify(provider string, err error, stderr []byte, elapsed time.Duration) (class ErrClass, inferred bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == approvalExitCode {
		return ApprovalTimeout, false
	}
	ph, known := providerPhrases[provider]
	if !known {
		return Other, false
	}
	text := strings.ToLower(string(stderr))
	has := func(list []string) bool {
		for _, p := range list {
			if strings.Contains(text, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has(ph.approval):
		return ApprovalTimeout, false
	case has(ph.signedIn):
		return NotSignedIn, false
	case has(ph.notFound):
		return NotFound, false
	case has(otherFailures):
		return Other, false
	case interactiveProvider[provider] && elapsed >= approvalElapsed:
		return ApprovalTimeout, true
	}
	return Other, false
}

// classifiedError builds the error for a failed call; context cancellation
// is never an approval timeout.
func classifiedError(ctx context.Context, provider string, argv0 string, err error, out, stderr []byte, elapsed time.Duration) *ProviderError {
	class, inferred := Other, false
	if ctx.Err() == nil {
		class, inferred = classify(provider, err, stderr, elapsed)
	}
	pe := &ProviderError{Class: class, Provider: provider, Elapsed: elapsed, Inferred: inferred}
	if class == Other {
		pe.detail = commandError(argv0, err, out, stderr, nil)
	}
	return pe
}
