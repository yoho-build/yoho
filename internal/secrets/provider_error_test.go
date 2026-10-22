package secrets

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

// scriptRunner answers each call from a queue and can sleep, advancing a
// fake clock by the same time.
type scriptRunner struct {
	mu      sync.Mutex
	calls   []string
	answers []scriptAnswer
	now     time.Time
}

type scriptAnswer struct {
	out, stderr string
	err         error
	took        time.Duration // fake time the call takes
	sleep       time.Duration // real time the call blocks
}

func (r *scriptRunner) Run(_ context.Context, argv []string, _ []string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(argv, " "))
	a := scriptAnswer{err: errors.New("exit status 1")}
	if len(r.answers) > 0 {
		a, r.answers = r.answers[0], r.answers[1:]
	}
	r.now = r.now.Add(a.took)
	r.mu.Unlock()
	time.Sleep(a.sleep)
	return []byte(a.out), []byte(a.stderr), a.err
}

func (r *scriptRunner) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func opCfg(refs ...string) config.SecretsConfig {
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"v": {Type: "op"}},
		Values:    map[string]config.SecretValue{},
	}
	for i, r := range refs {
		cfg.Values[string(rune('A'+i))] = config.SecretValue{Provider: "v", Ref: r}
	}
	return cfg
}

func load(t *testing.T, cfg config.SecretsConfig, opts LoadOptions) (*Store, error) {
	t.Helper()
	opts.Getenv = noEnv
	return Load(context.Background(), t.TempDir(), "production", cfg, opts)
}

func asProviderError(t *testing.T, err error) *ProviderError {
	t.Helper()
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("not a ProviderError: %v", err)
	}
	return pe
}

func TestProviderFailureClasses(t *testing.T) {
	leak := "SECRET-VALUE-9f3"
	cases := []struct {
		name   string
		a      scriptAnswer
		class  ErrClass
		code   string
		hasMsg string
	}{
		{"approval stderr", scriptAnswer{stderr: "[ERROR] authorization timeout " + leak}, ApprovalTimeout, "secrets_approval_timeout", "approval timed out"},
		{"approval after 60s", scriptAnswer{stderr: "[ERROR] something " + leak, took: 60 * time.Second}, ApprovalTimeout, "secrets_approval_timeout", "waited 1m0s"},
		{"not signed in", scriptAnswer{stderr: "[ERROR] You are not currently signed in " + leak}, NotSignedIn, "secrets_not_signed_in", "not signed in"},
		{"not found", scriptAnswer{stderr: `[ERROR] "x" isn't an item in the "y" vault ` + leak}, NotFound, "secrets_item_not_found", "item not found"},
		{"other", scriptAnswer{stderr: "[ERROR] boom " + leak}, Other, "", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.a.err = errors.New("exit status 1")
			r := &scriptRunner{answers: []scriptAnswer{tc.a}}
			_, err := load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Clock: r.clock})
			pe := asProviderError(t, err)
			if pe.Class != tc.class || pe.ErrorCode() != tc.code || !strings.Contains(err.Error(), tc.hasMsg) {
				t.Fatalf("class %v code %q err %v", pe.Class, pe.ErrorCode(), err)
			}
			if tc.class != Other && strings.Contains(err.Error(), leak) {
				t.Fatalf("classified error leaks stderr: %v", err)
			}
		})
	}
}

func TestProviderErrorSafeUnderNonemptyContract(t *testing.T) {
	leak := "SECRET-VALUE-9f3"
	cfg := opCfg("op://v/i/f")
	cfg.Nonempty = []string{"A"}
	r := &scriptRunner{answers: []scriptAnswer{{stderr: "authorization timeout " + leak, err: errors.New("exit status 1 " + leak)}}}
	_, err := load(t, cfg, LoadOptions{Runner: r})
	if pe := asProviderError(t, err); pe.Class != ApprovalTimeout || strings.Contains(err.Error(), leak) {
		t.Fatalf("got %v", err)
	}
	r = &scriptRunner{answers: []scriptAnswer{{stderr: "boom " + leak, err: errors.New("exit status 1")}}}
	_, err = load(t, cfg, LoadOptions{Runner: r})
	if err == nil || strings.Contains(err.Error(), leak) || errors.As(err, new(*ProviderError)) {
		t.Fatalf("Other must stay suppressed: %v", err)
	}
}

func TestCommandProviderExitCode75IsApprovalTimeout(t *testing.T) {
	ee := exec.Command("sh", "-c", "exit 75").Run()
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"c": {Type: "command", Command: []string{"my-provider"}}},
		Values:    map[string]config.SecretValue{"A": {Provider: "c", Ref: "x"}},
	}
	r := &scriptRunner{answers: []scriptAnswer{{err: ee}}}
	_, err := load(t, cfg, LoadOptions{Runner: r})
	if pe := asProviderError(t, err); pe.Class != ApprovalTimeout {
		t.Fatalf("got %v", err)
	}
	r = &scriptRunner{answers: []scriptAnswer{{stderr: "not signed in", err: errors.New("exit status 1")}}}
	_, err = load(t, cfg, LoadOptions{Runner: r})
	if pe := asProviderError(t, err); pe.Class != Other {
		t.Fatalf("command providers stay Other without exit 75: %v", err)
	}
}

func TestApprovalTimeoutRetriesOnceAndRerunsRunner(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{
		{stderr: "authorization timeout", err: errors.New("exit status 1")},
		{out: "the-value"},
	}}
	var warned []string
	s, err := load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Retries: 1, Warn: func(m string) { warned = append(warned, m) }})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("A"); v != "the-value" || len(r.calls) != 2 {
		t.Fatalf("value %q calls %v: the memo served the cached failure", v, r.calls)
	}
	if len(warned) != 1 || strings.Contains(warned[0], "the-value") {
		t.Fatalf("warned %v", warned)
	}

	// Retries are exhausted after Retries attempts.
	r = &scriptRunner{answers: []scriptAnswer{
		{stderr: "authorization timeout", err: errors.New("x")}, {stderr: "authorization timeout", err: errors.New("x")}, {out: "late"},
	}}
	if _, err = load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Retries: 1}); err == nil || len(r.calls) != 2 {
		t.Fatalf("err %v calls %v", err, r.calls)
	}

	// No retry without Retries, and none for other classes.
	r = &scriptRunner{answers: []scriptAnswer{{stderr: "authorization timeout", err: errors.New("x")}}}
	if _, err = load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r}); err == nil || len(r.calls) != 1 {
		t.Fatalf("err %v calls %v", err, r.calls)
	}
	r = &scriptRunner{answers: []scriptAnswer{{stderr: "not currently signed in", err: errors.New("x")}}}
	if _, err = load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Retries: 3}); err == nil || len(r.calls) != 1 {
		t.Fatalf("err %v calls %v", err, r.calls)
	}
}

func TestFirstProviderCallRunsAloneThenRest(t *testing.T) {
	var mu sync.Mutex
	active, maxActive, started := 0, 0, 0
	firstDone := false
	var early bool
	r := runnerFunc(func(argv []string) ([]byte, []byte, error) {
		mu.Lock()
		started++
		if started > 1 && !firstDone {
			early = true
		}
		active++
		if active > maxActive {
			maxActive = active
		}
		first := started == 1
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		if first {
			firstDone = true
		}
		mu.Unlock()
		return []byte("v"), nil, nil
	})
	if _, err := load(t, opCfg("op://a", "op://b", "op://c", "op://d"), LoadOptions{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if early || maxActive < 2 {
		t.Fatalf("early=%v maxActive=%d: first call must run alone, the rest in parallel", early, maxActive)
	}
}

func TestUnusableProviderStopsAfterFirstCall(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{{stderr: "authorization timeout", err: errors.New("x")}}}
	_, err := load(t, opCfg("op://a", "op://b", "op://c"), LoadOptions{Runner: r})
	if asProviderError(t, err).Class != ApprovalTimeout || len(r.calls) != 1 {
		t.Fatalf("err %v calls %v", err, r.calls)
	}
}

func TestOnWaitFiresOnceForSlowCall(t *testing.T) {
	var mu sync.Mutex
	var events []WaitEvent
	r := &scriptRunner{answers: []scriptAnswer{{out: "v", sleep: 120 * time.Millisecond}, {out: "w"}}}
	_, err := load(t, opCfg("op://v/slow/f", "op://v/fast/f"), LoadOptions{
		Runner: r, WaitAfter: 30 * time.Millisecond,
		OnWait: func(e WaitEvent) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Provider != "op" || events[0].Key != "A" || events[0].Elapsed < 30*time.Millisecond {
		t.Fatalf("events %+v", events)
	}
}

func TestOnWaitSilentForFastCall(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{{out: "v"}}}
	n := 0
	if _, err := load(t, opCfg("op://x"), LoadOptions{Runner: r, WaitAfter: time.Second, OnWait: func(WaitEvent) { n++ }}); err != nil || n != 0 {
		t.Fatalf("err %v events %d", err, n)
	}
}

type runnerFunc func(argv []string) ([]byte, []byte, error)

func (f runnerFunc) Run(_ context.Context, argv []string, _ []string) ([]byte, []byte, error) {
	return f(argv)
}

func TestSharedMemoRetryRunsOncePerRound(t *testing.T) {
	// Four keys share one ref, so one memoized call; all of them see the
	// approval timeout and retry, but the runner must run only twice.
	var mu sync.Mutex
	calls := 0
	r := runnerFunc(func(argv []string) ([]byte, []byte, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		if n == 1 {
			return nil, []byte("authorization timeout"), errors.New("exit status 1")
		}
		return []byte("v"), nil, nil
	})
	cfg := opCfg("op://same", "op://same", "op://same", "op://same")
	// The first key resolves alone and retries before the rest start, so
	// make every key's first attempt concurrent via the lower-level resolver.
	run := &memoRunner{r: r, calls: map[string]*memoCall{}}
	res := &providerResolver{run: run, getenv: noEnv, retries: 1}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = res.resolve(context.Background(), "K", cfg.Providers["v"], "op://same")
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("caller %d: %v", i, e)
		}
	}
	if calls != 2 {
		t.Fatalf("runner ran %d times, want 2 (one failure, one retry)", calls)
	}
}

// Permission and network errors are not approval timeouts, even after a long
// wait, and only op / bw get the wait heuristic.
func TestClassifyKeepsPermissionAndNetworkErrorsApart(t *testing.T) {
	long := 60 * time.Second
	exit1 := errors.New("exit status 1")
	cases := []struct {
		provider, stderr string
		elapsed          time.Duration
		class            ErrClass
		inferred         bool
	}{
		{"op", "[ERROR] You are not authorized to access this item", 0, Other, false},
		{"op", "[ERROR] You are not authorized to access this item", long, Other, false},
		{"op", "[ERROR] request timed out", long, Other, false},
		{"bw", "Request timed out", long, Other, false},
		{"op", "[ERROR] authorization timeout", 0, ApprovalTimeout, false},
		{"op", "[ERROR] something unexpected", long, ApprovalTimeout, true},
		{"bw", "", long, ApprovalTimeout, true},
		{"bws", "", long, Other, false},
		{"bws", "Error: 0: Received error message from server: [401 Unauthorized] {}", 0, NotSignedIn, false},
		{"bws", "Error: 0: Access token is not in a valid format: Doesn't contain a decryption key", 0, NotSignedIn, false},
		{"bws", "Error: 0: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\"}", 0, NotFound, false},
		{"bws", "error 401 in request id 404", 0, Other, false},
		{"bws", "bad access token value", 0, Other, false},
	}
	for _, tc := range cases {
		c, inf := classify(tc.provider, exit1, []byte(tc.stderr), tc.elapsed)
		if c != tc.class || inf != tc.inferred {
			t.Errorf("%s %q after %v: class %v inferred %v, want %v %v", tc.provider, tc.stderr, tc.elapsed, c, inf, tc.class, tc.inferred)
		}
	}
}

func TestPermissionErrorIsNotRetried(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{{stderr: "[ERROR] You are not authorized to access this item", err: errors.New("exit status 1"), took: 60 * time.Second}}}
	_, err := load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Clock: r.clock, Retries: 1})
	if err == nil || len(r.calls) != 1 {
		t.Fatalf("calls %d err %v", len(r.calls), err)
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.Class == ApprovalTimeout {
		t.Fatalf("permission error classified as approval: %v", err)
	}
}

func TestInferredApprovalTimeoutSaysSo(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{{stderr: "[ERROR] odd", err: errors.New("exit status 1"), took: 60 * time.Second}, {stderr: "[ERROR] odd", err: errors.New("exit status 1"), took: 60 * time.Second}}}
	_, err := load(t, opCfg("op://v/i/f"), LoadOptions{Runner: r, Clock: r.clock})
	pe := asProviderError(t, err)
	if pe.Class != ApprovalTimeout || !pe.Inferred || !strings.Contains(err.Error(), "likely an unanswered approval prompt") {
		t.Fatalf("got %+v: %v", pe, err)
	}
}
