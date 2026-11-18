package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/probe"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// tunnelRouteOptions are the flags of `yoho tunnel route`.
type tunnelRouteOptions struct {
	hostname  string
	path      string
	to        string
	checkPath string
	dryRun    bool
	timeout   time.Duration
}

// Timing of `yoho tunnel route`; variables so tests don't wait.
var (
	tunnelPrecheckTimeout = 15 * time.Second
	tunnelPublicInterval  = 2 * time.Second
	tunnelUndoTimeout     = 30 * time.Second
	tunnelWarnTimeout     = 10 * time.Second
)

// tunnelPublicCheck makes one request to the public URL from this machine;
// tests replace it.
var tunnelPublicCheck = publicCheck

// tunnelWarnSecrets resolves the secrets for the rollback ingress warning:
// no retries and no wait messages. Tests replace it.
var tunnelWarnSecrets = func(ctx context.Context, a *app) (*secrets.Store, error) {
	return secrets.Load(ctx, a.dir, a.sourceDestination(), a.cfg.Secrets, secrets.LoadOptions{})
}

var (
	dnsHostname  = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	httpStatusRe = regexp.MustCompile(`^http_status:[1-5][0-9][0-9]$`)
	tunnelNameRe = regexp.MustCompile(`^` + proxy.TunnelContainer + `(-[0-9]+)?$`)
	shellSafe    = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
)

func tunnelRouteCmd(g *globals) *cobra.Command {
	var o tunnelRouteOptions
	c := &cobra.Command{
		Use:   "route --hostname HOST --to proxy|ORIGIN",
		Short: "Point one hostname's ingress rule of a remotely managed tunnel at the Proxy, with checks and an automatic undo",
		Long: `Change the service of one ingress rule of the remotely managed Cloudflare
Tunnel named by proxy.tunnel.token_secret, through the Cloudflare API
(proxy.tunnel.api_token_secret, or migrate.tunnel.api_token_secret). Every
other rule, the catch-all and the rule's other settings stay as they are.

With --to proxy, Yoho first checks, before changing anything, that the
Proxy has this App Destination's route for the hostname and that the route
answers its health path (Host: HOST, X-Forwarded-Proto: https) from a Docker
network the cloudflared connector shares with yoho-proxy.

Before the change Yoho prints the old rule and the command that puts it
back, and saves both in <app dir>/tunnel-route.json on the Server. After
the change it requests https://HOST<health path> from this machine until it
answers 2xx or --timeout passes. When that fails, or the command is
interrupted, it puts the old service back on that one rule, unless someone
changed the rule since; then it leaves the rule alone and says so.

Wildcard hostnames are refused. --path selects a rule by its path, exactly
as the rule has it, when the hostname has several.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.tunnelRoute(cmd.Context(), o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.hostname, "hostname", "", "Public hostname of the ingress rule (required)")
	f.StringVar(&o.to, "to", "", "proxy (http://yoho-proxy:80) or an origin such as http://web:3000 (required)")
	f.StringVar(&o.path, "path", "", "Path of the rule, exactly as the rule has it, when the hostname has several rules")
	f.StringVar(&o.checkPath, "check-path", "", "Path the checks request (default: the route's health path, /up; / for other origins)")
	f.BoolVar(&o.dryRun, "dry-run", false, "Run the checks and show the change and the undo command; change nothing")
	f.DurationVar(&o.timeout, "timeout", 60*time.Second, "How long the public check waits for a 2xx after the change")
	_ = c.MarkFlagRequired("hostname")
	_ = c.MarkFlagRequired("to")
	addLockWaitFlag(c)
	return c
}

// tunnelAPITokenSecret is proxy.tunnel.api_token_secret, else the
// Destination's migrate.tunnel.api_token_secret.
func (a *app) tunnelAPITokenSecret() string {
	if t := a.proxyConfig().Tunnel; t != nil && t.APITokenSecret != "" {
		return t.APITokenSecret
	}
	if m := a.dest.Migrate; m != nil && m.Tunnel != nil {
		return m.Tunnel.APITokenSecret
	}
	return ""
}

// tunnelRouteAPI resolves both tokens and returns the client and the
// values to redact from errors.
func (a *app) tunnelRouteAPI(store *secrets.Store) (migrate.CloudflareAPI, []string, error) {
	t := a.proxyConfig().Tunnel
	if t == nil || t.TokenSecret == "" {
		return nil, nil, &hintError{"`yoho tunnel route` needs a remotely managed tunnel: proxy.tunnel.token_secret names the account and tunnel", "set proxy.tunnel.token_secret (the token from Cloudflare Zero Trust > Networks > Tunnels); Quick Tunnels have no ingress rules"}
	}
	key := a.tunnelAPITokenSecret()
	if key == "" {
		return nil, nil, &hintError{"no Cloudflare API token configured", "set proxy.tunnel.api_token_secret: CF_API_TOKEN and add CF_API_TOKEN to .yoho/secrets: a Cloudflare API token with Account > Cloudflare Tunnel > Edit"}
	}
	apiToken, ok := store.Get(key)
	if !ok || strings.TrimSpace(apiToken) == "" {
		return nil, nil, &hintError{fmt.Sprintf("api_token_secret %s is missing or empty", key), fmt.Sprintf("add %s to .yoho/secrets: a Cloudflare API token with Account > Cloudflare Tunnel > Edit", key)}
	}
	tunnelTok, err := tunnelToken(store, t)
	if err != nil {
		return nil, nil, err
	}
	cf, err := newTunnelAPI(strings.TrimSpace(apiToken), tunnelTok)
	return cf, []string{apiToken, strings.TrimSpace(apiToken), tunnelTok}, err
}

// tunnelTarget resolves --to: proxy, or an origin service as Cloudflare
// ingress rules write it.
func tunnelTarget(to string) (string, error) {
	to = strings.TrimSpace(to)
	if strings.EqualFold(to, "proxy") {
		return proxy.TunnelOrigin, nil
	}
	bad := fmt.Errorf("--to %q must be proxy or an origin such as http://web:3000", to)
	if to == "" || strings.IndexFunc(to, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", bad
	}
	if to == "hello_world" || httpStatusRe.MatchString(to) {
		return to, nil
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Opaque == "" && u.Path == "") {
		return "", bad
	}
	return to, nil
}

// sameOrigin compares two ingress services, so http://yoho-proxy and
// http://yoho-proxy:80/ are the same.
func sameOrigin(a, b string) bool { return normalizeOrigin(a) == normalizeOrigin(b) }

func normalizeOrigin(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		}
	}
	host := strings.ToLower(u.Hostname())
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
}

func shellArg(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return remote.Quote(s)
}

func checkPathOK(p string) error {
	return probe.Check{Type: probe.HTTP, Port: 80, Path: p}.Validate()
}

// tunnelRoute is `yoho tunnel route`.
func (a *app) tunnelRoute(ctx context.Context, o tunnelRouteOptions) error {
	o.hostname = strings.TrimSuffix(strings.TrimSpace(o.hostname), ".")
	if strings.HasPrefix(o.hostname, "*") {
		return a.fail(&hintError{fmt.Sprintf("--hostname %s is a wildcard: Yoho cannot check a wildcard from the outside", o.hostname), "change a wildcard rule in the Cloudflare dashboard, or add a rule for one hostname and route that"})
	}
	if !dnsHostname.MatchString(o.hostname) || len(o.hostname) > 253 {
		return a.fail(&hintError{fmt.Sprintf("--hostname %q is not a DNS hostname", o.hostname), "pass the public hostname alone, such as app.example.com (no scheme or path)"})
	}
	if o.checkPath != "" {
		if err := checkPathOK(o.checkPath); err != nil {
			return a.fail(&hintError{"--check-path: " + err.Error(), "pass an absolute path such as /up"})
		}
	}
	if o.timeout <= 0 {
		return a.fail(errors.New("--timeout must be positive"))
	}
	target, err := tunnelTarget(o.to)
	if err != nil {
		return a.fail(err)
	}

	a.ui.Title("Routing %s to %s on %s", o.hostname, target, a.destName)
	step := a.ui.Step("", "Resolve Cloudflare tokens")
	store, err := a.loadSecrets(ctx, step)
	var cf migrate.CloudflareAPI
	var redact []string
	if err == nil {
		cf, redact, err = a.tunnelRouteAPI(store)
	}
	if err != nil {
		hint := ""
		var he *hintError
		if errors.As(err, &he) {
			hint = he.hint
		}
		step.Fail(err, hint)
		return &silentError{err}
	}
	step.Done()
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	r := &tunnelRouter{a: a, hosts: hosts, cf: cf, o: o, target: target, redact: func(err error) error { return secrets.RedactError(err, redact...) }}
	return r.run(ctx)
}

// tunnelRouter runs one `yoho tunnel route` against connected Servers.
type tunnelRouter struct {
	a      *app
	hosts  []plan.NamedHost
	cf     migrate.CloudflareAPI
	o      tunnelRouteOptions
	target string
	redact func(error) error
}

// tunnelRouteRecord is <app dir>/tunnel-route.json: the last ingress change
// and how to undo it.
type tunnelRouteRecord struct {
	Hostname   string          `json:"hostname"`
	Path       string          `json:"path,omitempty"`
	OldRule    json.RawMessage `json:"old_rule"`
	OldService string          `json:"old_service"`
	NewService string          `json:"new_service"`
	BeforeHash string          `json:"before_hash"`
	AfterHash  string          `json:"after_hash"`
	Undo       string          `json:"undo"`
	Performer  string          `json:"performer"`
	Time       time.Time       `json:"time"`
	// Status: switching, done, undone, undo_conflict or undo_failed.
	Status string `json:"status"`
}

// TunnelRoutePath is where the last `yoho tunnel route` change is recorded.
func tunnelRoutePath(app, dest string) string {
	return path.Join(release.AppDir(app, dest), "tunnel-route.json")
}

func (r *tunnelRouter) event(phase, human string, fields map[string]any) {
	f := map[string]any{"phase": phase, "destination": r.a.destName, "hostname": r.o.hostname}
	if r.o.path != "" {
		f["path"] = r.o.path
	}
	for k, v := range fields {
		f[k] = v
	}
	r.a.ui.Event("tunnel_route", human, f)
}

func (r *tunnelRouter) fail(step interface{ Fail(error, string) }, err error) error {
	err = r.redact(err)
	msg, hint, _ := strings.Cut(err.Error(), "\nhint: ")
	if hint != "" {
		err = errors.New(msg)
	}
	step.Fail(err, hint)
	r.a.ui.Finished(err, "")
	return &silentError{err}
}

func (r *tunnelRouter) undoCommand(service, checkPath string) string {
	parts := []string{"yoho tunnel route -d", shellArg(r.a.destName), "--hostname", shellArg(r.o.hostname)}
	if r.o.path != "" {
		parts = append(parts, "--path", shellArg(r.o.path))
	}
	parts = append(parts, "--to", shellArg(service))
	if checkPath != "" {
		parts = append(parts, "--check-path", shellArg(checkPath))
	}
	return strings.Join(parts, " ")
}

func (r *tunnelRouter) run(ctx context.Context) error {
	a, o := r.a, r.o
	h0 := r.hosts[0]
	where := o.hostname
	if o.path != "" {
		where += " path " + o.path
	}

	step := a.ui.Step("", "Read tunnel ingress")
	live, err := r.cf.GetConfig(ctx)
	if err != nil {
		return r.fail(step, err)
	}
	old, oldRaw, err := migrate.FindRule(live, o.hostname, o.path)
	if err != nil {
		return r.fail(step, err)
	}
	if sameOrigin(old.Service, r.target) {
		step.Done(where + " → " + old.Service)
		r.event("already_routed", "", map[string]any{"service": old.Service})
		a.ui.Finished(nil, fmt.Sprintf("%s already routes to %s; nothing changed", where, old.Service))
		return nil
	}
	want, _, err := migrate.RewriteRule(live, o.hostname, o.path, r.target)
	if err != nil {
		return r.fail(step, err)
	}
	step.Done(where + " → " + old.Service)
	if rules, err := migrate.Rules(live); err == nil {
		for _, x := range rules {
			if strings.EqualFold(x.Hostname, o.hostname) && x.Path != o.path {
				p := x.Path
				if p == "" {
					p = "(no path)"
				}
				a.ui.Warn("the rule for %s path %s keeps %s; route it with --path %s", o.hostname, p, x.Service, shellArg(x.Path))
			}
		}
	}

	toProxy := sameOrigin(r.target, proxy.TunnelOrigin)
	checkHost := o.hostname
	if hh := originHostHeader(oldRaw); hh != "" && !strings.EqualFold(hh, o.hostname) {
		a.ui.Warn("the rule sets originRequest.httpHostHeader %s: the origin sees that Host, not %s", hh, o.hostname)
		checkHost = hh
	}
	checkPath := o.checkPath
	if toProxy {
		for _, h := range r.hosts {
			start := time.Now()
			step := a.ui.Step(h.Name, "Check %s through the Proxy", checkHost)
			res, err := r.precheck(ctx, h, checkHost, o.checkPath)
			if err != nil {
				err = fmt.Errorf("%w\nhint: %s", err, precheckHint(err))
				return r.fail(step, fmt.Errorf("nothing was changed: %w", err))
			}
			if checkPath == "" {
				checkPath = res.path
			}
			step.Done(fmt.Sprintf("route %s, GET %s via %s on %s", res.route, res.path, proxy.ContainerName, res.network))
			r.event("prechecked", "", map[string]any{"server": h.Name, "route": res.route, "check_path": res.path, "network": res.network, "duration_ms": time.Since(start).Milliseconds()})
		}
	} else {
		a.ui.Info("  --to is not the Proxy: no check before the change")
	}
	if checkPath == "" {
		checkPath = "/"
	}

	undo := r.undoCommand(old.Service, checkPath)
	var oldRule any
	_ = json.Unmarshal(oldRaw, &oldRule)
	r.event("old_rule", fmt.Sprintf("  Old rule: %s → %s %s\n  Undo:     %s", where, old.Service, string(oldRaw), undo),
		map[string]any{"old_service": old.Service, "new_service": r.target, "old_rule": oldRule, "undo": undo})
	if o.dryRun {
		r.event("dry_run", "", map[string]any{"new_service": r.target})
		a.ui.Finished(nil, fmt.Sprintf("Dry run: would route %s from %s to %s; nothing changed", where, old.Service, r.target))
		return nil
	}

	step = a.ui.Step(h0.Name, "Acquire lock")
	unlock, err := a.acquireLock(ctx, h0.Host, deploy.LockInfo{Command: "tunnel route"})
	if err != nil {
		step.Fail(err, hintFor(err, "check that the Server is reachable"))
		a.ui.Finished(err, "")
		return &silentError{err}
	}
	step.Done()
	defer func() {
		if uerr := unlock(context.WithoutCancel(ctx)); uerr != nil {
			a.ui.Warn("%v", uerr)
		}
	}()

	rec := &tunnelRouteRecord{
		Hostname: o.hostname, Path: o.path, OldRule: oldRaw, OldService: old.Service, NewService: r.target,
		BeforeHash: migrate.Hash(live), AfterHash: migrate.Hash(want), Undo: undo, Performer: performer(),
		Time: time.Now().UTC(), Status: "switching",
	}
	step = a.ui.Step(h0.Name, "Save undo record")
	if err := deploy.WriteJSON(ctx, h0.Host, tunnelRoutePath(a.cfg.App, a.destName), rec); err != nil {
		return r.fail(step, fmt.Errorf("nothing was changed: %w", err))
	}
	step.Done(tunnelRoutePath(a.cfg.App, a.destName))

	// From here on the rule may have changed: every failure undoes.
	start := time.Now()
	step = a.ui.Step("", "Switch %s to %s", where, r.target)
	err = r.cf.PutConfig(ctx, want)
	if err == nil {
		err = r.verifySwitched(ctx)
	}
	if err != nil {
		step.Fail(r.redact(err), "")
		return r.undo(ctx, rec, err)
	}
	step.Done()
	r.event("switched", "", map[string]any{"old_service": old.Service, "new_service": r.target, "duration_ms": time.Since(start).Milliseconds()})

	start = time.Now()
	u := "https://" + o.hostname + checkPath
	step = a.ui.Step("", "Check %s", u)
	if err := r.publicCheck(ctx, u); err != nil {
		step.Fail(err, "")
		return r.undo(ctx, rec, err)
	}
	step.Done()
	r.event("checked", "", map[string]any{"url": u, "duration_ms": time.Since(start).Milliseconds()})
	rec.Status = "done"
	r.saveRecord(ctx, rec)
	a.ui.Finished(nil, fmt.Sprintf("%s now routes to %s (undo: %s)", where, r.target, undo))
	return nil
}

// verifySwitched reads the configuration back.
func (r *tunnelRouter) verifySwitched(ctx context.Context) error {
	after, err := r.cf.GetConfig(ctx)
	if err != nil {
		return err
	}
	got, _, err := migrate.FindRule(after, r.o.hostname, r.o.path)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if !sameOrigin(got.Service, r.target) {
		return fmt.Errorf("read back: the rule points at %s, not %s", got.Service, r.target)
	}
	return nil
}

func (r *tunnelRouter) saveRecord(ctx context.Context, rec *tunnelRouteRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := deploy.WriteJSON(ctx, r.hosts[0].Host, tunnelRoutePath(r.a.cfg.App, r.a.destName), rec); err != nil {
		r.a.ui.Warn("update %s: %v", tunnelRoutePath(r.a.cfg.App, r.a.destName), err)
	}
}

// undo puts the old service back on the selected rule of the live
// configuration, only when the rule still has the service this run set.
// It returns cause, with what the undo did.
func (r *tunnelRouter) undo(ctx context.Context, rec *tunnelRouteRecord, cause error) error {
	a, o := r.a, r.o
	if ctx.Err() != nil {
		cause = fmt.Errorf("interrupted: %w", cause)
	}
	cause = r.redact(cause)
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tunnelUndoTimeout)
	defer cancel()
	step := a.ui.Step("", "Undo: put %s back on %s", rec.OldService, o.hostname)
	outcome, uerr := r.restore(uctx, rec)
	var err error
	switch {
	case uerr != nil:
		uerr = r.redact(uerr)
		rec.Status = "undo_failed"
		step.Fail(uerr, "run: "+rec.Undo)
		err = fmt.Errorf("%w; the undo failed: %v\nhint: put the old rule back with `%s`", cause, uerr, rec.Undo)
	case outcome == "conflict":
		rec.Status = "undo_conflict"
		cur := r.currentService(uctx)
		cerr := fmt.Errorf("the rule now points at %s, which this command did not set; left as is", cur)
		step.Fail(cerr, "")
		err = fmt.Errorf("%w; not undone: %v\nhint: someone changed the rule meanwhile; check it in the Cloudflare dashboard (the old rule is in %s)", cause, cerr, tunnelRoutePath(a.cfg.App, a.destName))
	case outcome == "unchanged":
		rec.Status = "undone"
		step.Done("the switch never landed")
		err = fmt.Errorf("%w; the ingress rule still points at %s\nhint: fix the problem, then run this again", cause, rec.OldService)
	default:
		rec.Status = "undone"
		step.Done(outcome)
		err = fmt.Errorf("%w; the ingress rule was put back to %s\nhint: fix the check, then run this again", cause, rec.OldService)
	}
	r.event("undo", "", map[string]any{"status": rec.Status, "old_service": rec.OldService})
	r.saveRecord(ctx, rec)
	msg, hint, _ := strings.Cut(err.Error(), "\nhint: ")
	a.ui.Error(errors.New(msg), hint)
	a.ui.Finished(errors.New(msg), "")
	return &silentError{err}
}

// restore returns "restored", "unchanged" (the switch never landed) or
// "conflict".
func (r *tunnelRouter) restore(ctx context.Context, rec *tunnelRouteRecord) (string, error) {
	live, err := r.cf.GetConfig(ctx)
	if err != nil {
		return "", err
	}
	cur, _, err := migrate.FindRule(live, r.o.hostname, r.o.path)
	if err != nil {
		return "conflict", nil
	}
	switch {
	case sameOrigin(cur.Service, rec.NewService):
	case sameOrigin(cur.Service, rec.OldService):
		return "unchanged", nil
	default:
		return "conflict", nil
	}
	back, _, err := migrate.RewriteRule(live, r.o.hostname, r.o.path, rec.OldService)
	if err != nil {
		return "", err
	}
	if err := r.cf.PutConfig(ctx, back); err != nil {
		return "", err
	}
	return "restored", nil
}

func (r *tunnelRouter) currentService(ctx context.Context) string {
	live, err := r.cf.GetConfig(ctx)
	if err != nil {
		return "something else"
	}
	cur, _, err := migrate.FindRule(live, r.o.hostname, r.o.path)
	if err != nil {
		return "nothing (the rule is gone)"
	}
	return cur.Service
}

// errAccessRedirect: Cloudflare Access answers before the origin.
var errAccessRedirect = errors.New("Cloudflare Access redirected to its login page")

// publicCheck makes one GET of u from this machine without following
// redirects; 2xx is success.
func publicCheck(ctx context.Context, u string) error {
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "yoho-tunnel-route")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if lu, err := url.Parse(loc); err == nil && (strings.HasSuffix(strings.ToLower(lu.Hostname()), ".cloudflareaccess.com")) {
			return errAccessRedirect
		}
		return fmt.Errorf("got %s (Location: %s), want 2xx", resp.Status, loc)
	}
	return fmt.Errorf("got %s, want 2xx", resp.Status)
}

// publicCheck polls tunnelPublicCheck until it succeeds or --timeout ends
// (edge configuration takes a few seconds to reach the connectors).
func (r *tunnelRouter) publicCheck(ctx context.Context, u string) error {
	pctx, cancel := context.WithTimeout(ctx, r.o.timeout)
	defer cancel()
	tries := 0
	err := probe.Poll(pctx, tunnelPublicInterval, func(ctx context.Context) error {
		tries++
		err := tunnelPublicCheck(ctx, u)
		if errors.Is(err, errAccessRedirect) {
			cancel()
		}
		return err
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errAccessRedirect):
		return fmt.Errorf("%s: %w\nhint: Yoho cannot check a hostname behind Cloudflare Access; add a bypass policy for the check path, or check it yourself and route with the dashboard", u, err)
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", u, ctx.Err())
	}
	return fmt.Errorf("%s: no 2xx in %s (%s): %w", u, r.o.timeout, plural(tries, "attempt", "attempts"), err)
}

// precheckResult is what the pre-check found on one Server.
type precheckResult struct {
	route, path, network, ip string
}

// precheck: the Proxy has this App Destination's route for host, and the
// route answers its health path from the network cloudflared shares with
// yoho-proxy, dialed through the Server.
func (r *tunnelRouter) precheck(ctx context.Context, h plan.NamedHost, host, checkPath string) (precheckResult, error) {
	ctx, cancel := context.WithTimeout(ctx, tunnelPrecheckTimeout)
	defer cancel()
	a := r.a
	var res precheckResult
	routes, err := proxy.List(ctx, h.Host)
	if err != nil {
		return res, err
	}
	owner := a.cfg.App + "-" + a.destName + "-"
	rt, err := proxyRouteFor(routes, owner, host)
	if err != nil {
		return res, err
	}
	res.route = rt.Service
	res.path = checkPath
	if res.path == "" {
		res.path, err = currentHealthPath(ctx, h.Host, a.cfg.App, a.destName, strings.TrimPrefix(rt.Service, owner))
		if err != nil {
			return res, err
		}
	}
	res.network, res.ip, err = proxyAddrForConnector(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		return res, err
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return remote.Dial(ctx, h.Host, network, addr)
	}
	// cloudflared reaches the Proxy at TunnelOrigin, port 80.
	c := probe.Check{Type: probe.HTTP, Port: 80, Path: res.path, Host: host}
	hdr := http.Header{"X-Forwarded-Proto": {"https"}}
	err = probe.Poll(ctx, time.Second, func(ctx context.Context) error {
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		defer acancel()
		return probe.Run(actx, dial, res.ip, c, hdr)
	})
	if err != nil {
		return res, fmt.Errorf("%s through %s on %s: %w", rt.Service, proxy.ContainerName, res.network, err)
	}
	return res, nil
}

func precheckHint(err error) string {
	var fe *remote.ForwardError
	if errors.As(err, &fe) && fe.Hint != "" {
		return fe.Hint
	}
	if errors.Is(err, errNoRoute) {
		return "deploy the App with x-yoho.proxy.hosts listing the hostname first"
	}
	return "fix the route (see `yoho proxy status` and `yoho app logs`), or pass --check-path for a path that answers 200-399"
}

var errNoRoute = errors.New("no Proxy route")

func routeHasHost(rt proxy.Route, host string) bool {
	for _, h := range rt.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
		if suffix, ok := strings.CutPrefix(h, "*."); ok && strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

func catchAll(rt proxy.Route) bool {
	for _, h := range rt.Hosts {
		if h != "" {
			return false
		}
	}
	return true
}

// proxyRouteFor picks the Proxy route that serves host: a route naming the
// host, else a catch-all. It must belong to this App Destination (owner is
// the route name prefix).
func proxyRouteFor(routes []proxy.Route, owner, host string) (proxy.Route, error) {
	for _, pick := range []func(proxy.Route) bool{
		func(rt proxy.Route) bool { return routeHasHost(rt, host) },
		catchAll,
	} {
		var ours, theirs []proxy.Route
		for _, rt := range routes {
			if !pick(rt) {
				continue
			}
			if strings.HasPrefix(rt.Service, owner) {
				ours = append(ours, rt)
			} else {
				theirs = append(theirs, rt)
			}
		}
		if len(ours) == 0 && len(theirs) > 0 {
			return proxy.Route{}, fmt.Errorf("the Proxy serves %s with route %s, which is not this App Destination's", host, theirs[0].Service)
		}
		if len(ours) > 0 {
			sort.SliceStable(ours, func(i, j int) bool { return rootRoute(ours[i]) && !rootRoute(ours[j]) })
			return ours[0], nil
		}
	}
	return proxy.Route{}, fmt.Errorf("%w for %s: the Proxy has no route of %s for it", errNoRoute, host, strings.TrimSuffix(owner, "-"))
}

func rootRoute(rt proxy.Route) bool {
	return len(rt.PathPrefixes) == 0 || slices.Contains(rt.PathPrefixes, "/")
}

// planProxy is the part of a Release's plan.json this file reads: compose
// writes a list of Services, swarm an object with them.
type planProxy struct {
	Name  string               `json:"name"`
	Proxy *config.ServiceProxy `json:"proxy,omitempty"`
}

func parsePlanProxies(b []byte) ([]planProxy, error) {
	var list []planProxy
	if err := json.Unmarshal(b, &list); err == nil {
		return list, nil
	}
	var st struct {
		Services []planProxy `json:"services"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse plan.json: %w", err)
	}
	return st.Services, nil
}

func readPlanProxies(ctx context.Context, h remote.Host, app, dest, version string) ([]planProxy, error) {
	b, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, version), "plan.json"), false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("Release %s has no plan.json", version)
		}
		return nil, err
	}
	return parsePlanProxies(b)
}

// currentHealthPath is the health path of service in the current Release.
func currentHealthPath(ctx context.Context, h remote.Host, app, dest, service string) (string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(release.AppDir(app, dest), "current")) + " 2>/dev/null || true"})
	if err != nil {
		return "", fmt.Errorf("read current Release: %w", err)
	}
	ver := path.Base(strings.TrimSpace(out))
	if ver == "" || ver == "." || ver == "/" {
		return "", fmt.Errorf("%s/%s has no current Release on %s; deploy it first", app, dest, h.Name())
	}
	svcs, err := readPlanProxies(ctx, h, app, dest, ver)
	if err != nil {
		return "", err
	}
	for _, s := range svcs {
		if s.Name == service && s.Proxy != nil && s.Proxy.HealthPath != "" {
			return s.Proxy.HealthPath, nil
		}
	}
	return "/up", nil
}

// networksScript prints, for yoho-proxy, the yoho-tunnel connectors and
// this App Destination's containers, one line each:
// /name|image|network=ip|network=ip...
func networksScript(app, dest string) string {
	format := `{{.Name}}|{{.Config.Image}}{{range $n, $v := .NetworkSettings.Networks}}|{{$n}}={{$v.IPAddress}}{{end}}`
	return "set -eu\n" +
		"ids=$( { docker ps -q --filter 'name=^/" + proxy.ContainerName + "$'; " +
		"docker ps -q --filter 'name=^/" + proxy.TunnelContainer + "(-[0-9]+)?$'; " +
		"docker ps -q --filter " + remote.Quote("label="+deploy.LabelApp+"="+app) + " --filter " + remote.Quote("label="+deploy.LabelDestination+"="+dest) + "; } | sort -u)\n" +
		"[ -z \"$ids\" ] || docker inspect --format " + remote.Quote(format) + " $ids\n"
}

type containerNets struct {
	name, image string
	nets        map[string]string
}

func parseNetworks(out string) []containerNets {
	var cs []containerNets
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "|")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		c := containerNets{name: strings.TrimPrefix(f[0], "/"), image: f[1], nets: map[string]string{}}
		for _, n := range f[2:] {
			if k, v, ok := strings.Cut(n, "="); ok && k != "" {
				c.nets[k] = v
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// proxyAddrForConnector finds the cloudflared connectors (the yoho-tunnel
// containers, else a cloudflared Service of this App Destination) and
// returns a network every one of them shares with yoho-proxy, and
// yoho-proxy's address on it.
func proxyAddrForConnector(ctx context.Context, h remote.Host, app, dest string) (string, string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: networksScript(app, dest)})
	if err != nil {
		return "", "", fmt.Errorf("inspect networks: %w", err)
	}
	var px *containerNets
	var managed, inApp []containerNets
	for _, c := range parseNetworks(out) {
		switch {
		case c.name == proxy.ContainerName:
			px = &c
		case tunnelNameRe.MatchString(c.name):
			managed = append(managed, c)
		case strings.Contains(strings.ToLower(c.image), "cloudflared"):
			inApp = append(inApp, c)
		}
	}
	if px == nil {
		return "", "", fmt.Errorf("%s is not running on %s", proxy.ContainerName, h.Name())
	}
	conns := managed
	if len(conns) == 0 {
		conns = inApp
	}
	if len(conns) == 0 {
		return "", "", fmt.Errorf("no cloudflared connector runs on %s (neither %s nor a cloudflared Service of %s/%s)", h.Name(), proxy.TunnelContainer, app, dest)
	}
	var shared []string
	for i, c := range conns {
		var mine []string
		for n := range c.nets {
			if ip := px.nets[n]; ip != "" {
				mine = append(mine, n)
			}
		}
		if len(mine) == 0 {
			return "", "", fmt.Errorf("connector %s shares no Docker network with %s, so it cannot reach %s", c.name, proxy.ContainerName, proxy.TunnelOrigin)
		}
		if i == 0 {
			shared = mine
			continue
		}
		var both []string
		for _, n := range shared {
			if slices.Contains(mine, n) {
				both = append(both, n)
			}
		}
		shared = both
	}
	if len(shared) == 0 {
		return "", "", fmt.Errorf("the cloudflared connectors on %s share no one Docker network with %s", h.Name(), proxy.ContainerName)
	}
	sort.Slice(shared, func(i, j int) bool {
		if (shared[i] == proxy.Network) != (shared[j] == proxy.Network) {
			return shared[i] == proxy.Network
		}
		return shared[i] < shared[j]
	})
	return shared[0], px.nets[shared[0]], nil
}

// originHostHeader is the rule's originRequest.httpHostHeader.
func originHostHeader(rule json.RawMessage) string {
	var r struct {
		OriginRequest struct {
			HTTPHostHeader string `json:"httpHostHeader"`
		} `json:"originRequest"`
	}
	_ = json.Unmarshal(rule, &r)
	return r.OriginRequest.HTTPHostHeader
}

// warnTunnelBypass warns, best effort, when a hostname the target Release
// routes through the Proxy has a tunnel ingress rule that points elsewhere:
// the rollback then does not change what that hostname serves. Only with
// proxy.tunnel.token_secret and an API token secret; bounded, never fails.
func (a *app) warnTunnelBypass(ctx context.Context, h remote.Host, target string) {
	t := a.proxyConfig().Tunnel
	if t == nil || t.TokenSecret == "" || a.tunnelAPITokenSecret() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, tunnelWarnTimeout)
	defer cancel()
	svcs, err := readPlanProxies(ctx, h, a.cfg.App, a.destName, target)
	if err != nil {
		return
	}
	var hosts []string
	seen := map[string]bool{}
	for _, s := range svcs {
		if s.Proxy == nil {
			continue
		}
		for _, hn := range s.Proxy.Hosts {
			if k := strings.ToLower(hn); hn != "" && !seen[k] {
				seen[k] = true
				hosts = append(hosts, hn)
			}
		}
	}
	if len(hosts) == 0 {
		return
	}
	store, err := tunnelWarnSecrets(ctx, a)
	if err != nil {
		return
	}
	cf, _, err := a.tunnelRouteAPI(store)
	if err != nil {
		return
	}
	live, err := cf.GetConfig(ctx)
	if err != nil {
		return
	}
	rules, err := migrate.Rules(live)
	if err != nil {
		return
	}
	for _, hn := range hosts {
		for _, rule := range rules {
			if !strings.EqualFold(rule.Hostname, hn) || sameOrigin(rule.Service, proxy.TunnelOrigin) {
				continue
			}
			where, flag := hn, ""
			if rule.Path != "" {
				where += " path " + rule.Path
				flag = " --path " + shellArg(rule.Path)
			}
			a.ui.Warn("Cloudflare Tunnel ingress for %s points at %s, not the Proxy: this rollback does not change what it serves; route it with `yoho tunnel route -d %s --hostname %s%s --to proxy`", where, rule.Service, shellArg(a.destName), hn, flag)
		}
	}
}
