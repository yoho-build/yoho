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

// approvalElapsed is when a failing op / bw / bws call is taken as an
// unanswered approval prompt: their own prompt timeout is about 60 s.
const approvalElapsed = 55 * time.Second

// ProviderError is a classified secrets provider failure. Its text is fixed
// per class and carries neither stderr nor stdout, so it is safe where
// command output is suppressed (the secrets.nonempty contract). Other keeps
// the redacted command error, which callers must not show under a contract.
type ProviderError struct {
	Class    ErrClass
	Provider string
	Elapsed  time.Duration
	// note is fixed text chosen by Yoho, never provider output.
	note   string
	detail error
}

func (e *ProviderError) Error() string {
	switch e.Class {
	case ApprovalTimeout:
		s := "secrets provider " + e.Provider + ": approval timed out or was not granted"
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
// likely waiting for an approval. Ref is a name, never a value.
type WaitEvent struct {
	Provider string
	Ref      string
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

var providerPhrases = map[string]struct{ approval, signedIn, notFound []string }{
	"op": {
		approval: []string{"authorization timeout", "authorization prompt", "authorization denied", "authorization was denied", "not authorized", "timed out"},
		signedIn: []string{"not signed in", "no accounts configured", "not currently signed in", "session expired", "sign in to"},
		notFound: []string{"isn't an item", "isn't a vault", "could not be found", "no item", "isn't a field", "does not exist"},
	},
	"bw": {
		approval: []string{"timed out", "authorization"},
		signedIn: []string{"not logged in", "vault is locked", "you are not logged in", "invalid master password", "bw_session"},
		notFound: []string{"not found"},
	},
	"bws": {
		approval: []string{"timed out", "authorization"},
		signedIn: []string{"access token", "unauthorized", "not logged in", "401"},
		notFound: []string{"not found", "404"},
	},
}

// classify places a provider failure. stderr is read here and dropped.
func classify(provider string, err error, stderr []byte, elapsed time.Duration) ErrClass {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == approvalExitCode {
		return ApprovalTimeout
	}
	ph, known := providerPhrases[provider]
	if !known {
		return Other
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
		return ApprovalTimeout
	case has(ph.signedIn):
		return NotSignedIn
	case has(ph.notFound):
		return NotFound
	case elapsed >= approvalElapsed:
		return ApprovalTimeout
	}
	return Other
}

// classifiedError builds the error for a failed call; context cancellation
// is never an approval timeout.
func classifiedError(ctx context.Context, provider string, argv0 string, err error, out, stderr []byte, elapsed time.Duration) *ProviderError {
	class := Other
	if ctx.Err() == nil {
		class = classify(provider, err, stderr, elapsed)
	}
	pe := &ProviderError{Class: class, Provider: provider, Elapsed: elapsed}
	if class == Other {
		pe.detail = commandError(argv0, err, out, stderr, nil)
	}
	return pe
}
