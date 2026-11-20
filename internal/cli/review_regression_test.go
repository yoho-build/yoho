package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/migrate/migratetest"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

func reviewCommand(g *globals, path string, run func(*cobra.Command, []string) error) (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "yoho", SilenceErrors: true, SilenceUsage: true}
	parent := root
	var leaf *cobra.Command
	for _, part := range strings.Fields(path) {
		leaf = &cobra.Command{Use: part}
		parent.AddCommand(leaf)
		parent = leaf
	}
	leaf.RunE = run
	return root, leaf
}
func TestReviewRawAndReadOnlyOutput(t *testing.T) {
	for _, tc := range []struct{ path, flag string }{{"app exec", ""}, {"app run", ""}, {"app stats", ""}, {"access audit", ""}, {"app logs", ""}, {"setup", "plan"}, {"setup", ""}, {"secrets rotate", "dry-run"}, {"tunnel route", "dry-run"}, {"init", "list-templates"}} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(tc.path+tc.flag+map[bool]string{true: "-json"}[jsonMode], func(t *testing.T) {
				g := &globals{json: jsonMode}
				root, leaf := reviewCommand(g, tc.path, func(cmd *cobra.Command, _ []string) error {
					io.WriteString(cmd.OutOrStdout(), "payload\n")
					g.ui(cmd).Finished(nil, "done")
					return nil
				})
				args := strings.Fields(tc.path)
				if tc.flag != "" {
					leaf.Flags().Bool(tc.flag, false, "")
					args = append(args, "--"+tc.flag)
				}
				var b bytes.Buffer
				root.SetOut(&b)
				auditWrap(g, root)
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if b.String() != "payload\n" {
					t.Fatalf("stream corrupted: %q", b.String())
				}
			})
		}
	}
}
func TestReviewPreMutationFailuresAreKnown(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	bin := t.TempDir()
	marker := filepath.Join(bin, "ssh-called")
	t.Setenv("REVIEW_SSH_MARKER", marker)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf called >> \"$REVIEW_SSH_MARKER\"\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configPath := appDir(t, "compose", "services:\n  web:\n    image: nginx\n")
	for _, tc := range []struct {
		err  error
		code int
	}{{context.Canceled, 1}, {&remote.ConnectError{ExitError: &remote.ExitError{Code: 255}}, 12}, {&validationError{errors.New("bad config")}, 10}} {
		g := &globals{json: true, configPath: configPath}
		root, _ := reviewCommand(g, "remove", func(cmd *cobra.Command, _ []string) error {
			if !preMutation(cmd.Context()) {
				t.Fatal("mutation started before validation/prompt")
			}
			return tc.err
		})
		var b bytes.Buffer
		root.SetOut(&b)
		auditWrap(g, root)
		root.SetArgs([]string{"remove"})
		err := root.Execute()
		if mutationCode(err, false) != tc.code || strings.Contains(b.String(), `"outcome":"unknown"`) {
			t.Fatalf("%v\n%s", err, b.String())
		}
	}
}

type reviewHost struct {
	remote.Local
	records  []operation.Record
	commands int
}

func (h *reviewHost) Run(_ context.Context, c remote.Cmd) error {
	h.commands++
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		var r operation.Record
		if json.Unmarshal(b, &r) == nil && r.State != "" {
			h.records = append(h.records, r)
		}
	}
	return nil
}
func TestReviewInterruptedRecordStaysRunning(t *testing.T) {
	for _, err := range []error{context.Canceled, &remote.ExitError{Code: 255}} {
		h := &reviewHost{}
		r := &auditRun{h: h, mutated: true, begin: time.Now(), red: audit.NewRedactor(), a: &app{ui: ui.Discard()}, rec: audit.Record{Run: "abc123", App: "shop", Destination: "production"}, op: &operation.Record{ID: "abc123", State: "running", Services: map[string]string{}}}
		r.finish(classifyMutation(err, true))
		if len(h.records) != 1 || h.records[0].State != "running" || h.records[0].Outcome != "unknown" {
			t.Fatalf("%+v", h.records)
		}
	}
}
func TestReviewRemovedOperationDoesNotRecreateDirectory(t *testing.T) {
	h := &reviewHost{}
	r := &auditRun{h: h, deleted: true, begin: time.Now(), red: audit.NewRedactor(), op: &operation.Record{ID: "abc123"}}
	r.finish(nil)
	if len(h.records) != 0 {
		t.Fatal("removed directory recreated")
	}
}
func TestReviewGateMutationAndErrorCodes(t *testing.T) {
	r := &auditRun{}
	ctx := context.WithValue(context.Background(), auditRunKey{}, r)
	if got := mutationCode(classifyOperation(ctx, context.Canceled), false); got != 1 {
		t.Fatal(got)
	}
	markScopedMutation(ctx)
	if got := mutationCode(classifyOperation(ctx, context.Canceled), false); got != 14 {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		err  *remote.GateError
		code int
	}{{&remote.GateError{Code: "locked"}, 11}, {&remote.GateError{Code: "invalid"}, 10}, {&remote.GateError{Code: "failed", Outcome: "partial", Changed: []string{"web"}}, 13}} {
		if got := mutationCode(classifyOperation(ctx, (&app{ui: ui.Discard()}).gateFail(tc.err)), false); got != tc.code {
			t.Fatalf("%+v: %d", tc, got)
		}
	}
}
func TestReviewCobraValidationJSON(t *testing.T) {
	for _, args := range [][]string{{"apply", "--plan", "p.json", "--version", "v1", "--json"}, {"deploy", "--refresh", "--skip-build", "--json"}, {"rollback", "--json"}, {"deploy", "--not-a-flag", "--json"}} {
		g := &globals{json: true}
		root := newRoot(g)
		var b bytes.Buffer
		root.SetOut(&b)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		err := root.Execute()
		if mutationCode(err, false) != 10 {
			t.Fatalf("%v: %v", args, err)
		}
		var event map[string]any
		if json.Unmarshal(bytes.TrimSpace(b.Bytes()), &event) != nil || event["event"] != "error" {
			t.Fatalf("want exactly one JSON error: %s", b.String())
		}
	}
}
func TestReviewPlanArtifactHashesReasonsWithoutCorruptingMetadata(t *testing.T) {
	p := validSavedPlan()
	p.Changes = []plan.Change{{Kind: "service", Name: "web", Action: plan.ActionUpdate, Reasons: []string{"token=2026"}}}
	p.ChangesHash, _ = digest(p.Changes)
	store := secrets.New(map[string]string{"PIN": "2026", "SHORT": p.Config})
	b, err := encodeSavedPlan(&p, store.RedactString)
	if err != nil {
		t.Fatal(err)
	}
	var approved savedPlan
	if err = json.Unmarshal(b, &approved); err != nil {
		t.Fatal(err)
	}
	if err = approved.compare(&p); err != nil {
		t.Fatal(err)
	}
	if approved.Created != p.Created || approved.Config != p.Config || bytes.Contains(b, []byte("token=2026")) {
		t.Fatalf("invalid metadata or leaked reason: %s", b)
	}
	p.Changes[0].Reasons = []string{"token=2027"}
	p.ChangesHash, _ = digest(p.Changes)
	if mutationCode(approved.compare(&p), false) != 15 {
		t.Fatal("changed reason accepted")
	}
}
func TestReviewSavedConfigIncludesAppFilesAndHooks(t *testing.T) {
	for _, kind := range []string{"bind", "config", "hook"} {
		t.Run(kind, func(t *testing.T) {
			source := "services:\n  web:\n    image: nginx\n"
			switch kind {
			case "bind":
				source += "    volumes: ['./content:/etc/content:ro']\n"
			case "config":
				source += "    configs: [content]\nconfigs:\n  content:\n    file: ./content\n"
			}
			file := appDir(t, "compose", source)
			dir := filepath.Dir(file)
			os.WriteFile(filepath.Join(dir, "content"), []byte("one"), 0600)
			os.MkdirAll(filepath.Join(dir, ".yoho/hooks"), 0700)
			hook := filepath.Join(dir, ".yoho/hooks/pre-deploy")
			os.WriteFile(hook, []byte("one"), 0700)
			cfg, err := config.Load(file)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := composefile.Load(context.Background(), dir, nil, nil, "shop", "")
			if err != nil {
				t.Fatal(err)
			}
			a := &app{cfg: cfg, dir: dir, destName: "production", dest: cfg.Destinations["production"]}
			s := &session{r: loaded, ver: "v1", store: secrets.New(nil)}
			before, err := a.snapshotPlan(context.Background(), s, nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "content")
			if kind == "hook" {
				target = hook
			}
			os.WriteFile(target, []byte("two"), 0600)
			after, err := a.snapshotPlan(context.Background(), s, nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if mutationCode(before.compare(after), false) != 15 || before.Config == after.Config {
				t.Fatal("shipped content change accepted")
			}
		})
	}
}
func TestReviewSavedPlanRefusesUnconvergedAppDir(t *testing.T) {
	if err := savedAppDirReady([]plan.Change{{Kind: "app_dir", Action: plan.ActionUpdate}}); mutationCode(err, false) != 10 || !strings.Contains(err.Error(), "yoho apply") {
		t.Fatal(err)
	}
}
func TestReviewSavedPlanVersionAndSilentErrors(t *testing.T) {
	p := validSavedPlan()
	file := filepath.Join(t.TempDir(), "plan.json")
	b, _ := json.Marshal(p)
	os.WriteFile(file, b, 0600)
	c := &cobra.Command{Use: "apply"}
	c.Flags().String("plan", file, "")
	a := &app{}
	if got := a.auditVersion(c, "apply", nil); got != p.Version {
		t.Fatal(got)
	}
	err := planOutputError(&silentError{stalePlan("bad")})
	var silent *silentError
	if !errors.As(err, &silent) {
		t.Fatal("step failure lost silent wrapper")
	}
}
func TestReviewReloadedSecretsRefreshRedactors(t *testing.T) {
	var b bytes.Buffer
	a := &app{ui: ui.New(&b, ui.JSON, false)}
	s := &session{a: a, store: secrets.New(map[string]string{"KEY": "sensitive-value"})}
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = original; reader.Close(); writer.Close() }()
	s.replaceStore(secrets.New(map[string]string{"KEY": "sensitive-value-extended"}))
	a.ui.Info("sensitive-value sensitive-value-extended")
	io.WriteString(s.red.(io.Writer), "sensitive-value-extended")
	s.red.Flush()
	writer.Close()
	out, _ := io.ReadAll(reader)
	if strings.Contains(b.String(), "sensitive-value") || strings.Contains(b.String(), "-extended") || bytes.Contains(out, []byte("sensitive-value-extended")) {
		t.Fatalf("secret leaked: %s %s", b.String(), out)
	}
}

func TestReviewSavedApprovalVerifiesBeforeEventsAndSkipsNoop(t *testing.T) {
	var b bytes.Buffer
	a := &app{cfg: &config.Config{App: "shop"}, destName: "production", g: &globals{json: true}, ui: ui.New(&b, ui.JSON, false)}
	if err := a.confirmPrecheck(applyOptions{PlanFile: "approved.json"}); err != nil {
		t.Fatalf("saved approval requested a second prompt: %v", err)
	}
	approved := validSavedPlan()
	current := approved
	current.Config = "drift"
	if _, err := a.verifyApproval(&approved, &current, nil); mutationCode(err, false) != 15 || b.Len() != 0 {
		t.Fatalf("unverified events: %s, %v", b.String(), err)
	}
	current = approved
	noop, err := a.verifyApproval(&approved, &current, []plan.Change{{Kind: "service", Name: "web", Action: plan.ActionNoop}})
	if err != nil || !noop {
		t.Fatalf("verified noop executed: %v", err)
	}
	if !strings.Contains(b.String(), `"event":"plan_verified"`) || strings.Contains(b.String(), `"event":"change"`) || strings.Contains(b.String(), "Deployed") {
		t.Fatal(b.String())
	}
}

func TestReviewCutoverRollbackRecordsMutation(t *testing.T) {
	ctx := context.Background()
	h := &migratetest.Host{}
	j := migrate.NewJournal("shop", "production", "compose", time.Now())
	if err := j.Save(ctx, h); err != nil {
		t.Fatal(err)
	}
	recorder := &reviewHost{}
	r := &auditRun{h: recorder, begin: time.Now(), rec: audit.Record{Run: "cutover123", Command: "rollback", App: "shop", Destination: "production"}}
	ctx = context.WithValue(ctx, auditRunKey{}, r)
	a := &app{cfg: &config.Config{App: "shop"}, destName: "production", ui: ui.Discard()}
	if err := a.rollbackCutoverOn(ctx, plan.NamedHost{Name: "one", Host: h}, secrets.New(nil), false); err != nil {
		t.Fatal(err)
	}
	if !r.mutated || len(recorder.records) == 0 {
		t.Fatal("cutover rollback ran without operation record")
	}
}

func TestOwnSummaryCommandsEndWithTheirSummary(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := nonemptyApp(t, "compose", "services:\n  web: {image: nginx, restart: unless-stopped}\n", "", "")
	g := &globals{configPath: p, json: true}
	root := newRoot(g)
	var b bytes.Buffer
	root.SetOut(&b)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"config", "check", "--offline", "--json", "-c", p})
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last["event"] != "check" {
		t.Fatalf("last event must be check, got:\n%s", b.String())
	}
	doctor, _, err := root.Find([]string{"doctor"})
	if err != nil || !ownSummaryCommand(doctor) {
		t.Fatalf("doctor must keep its own final summary event: %v", err)
	}
}
