package accesstoken

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Provider is the CI system a Target belongs to.
type Provider string

const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
)

// Target is a CI secret destination: github:OWNER/REPO[@ENV] or
// gitlab:GROUP/PROJECT[@ENV] (GitLab groups may nest).
type Target struct {
	Provider Provider
	Repo     string // OWNER/REPO or GROUP/PROJECT
	Env      string // optional environment
}

// ParseTarget parses "github:OWNER/REPO[@ENV]" or "gitlab:GROUP/PROJECT[@ENV]".
func ParseTarget(s string) (Target, error) {
	prov, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Target{}, fmt.Errorf("invalid target %q: want github:OWNER/REPO[@ENV] or gitlab:GROUP/PROJECT[@ENV]", s)
	}
	t := Target{Provider: Provider(prov)}
	if t.Provider != GitHub && t.Provider != GitLab {
		return Target{}, fmt.Errorf("invalid target %q: provider must be github or gitlab", s)
	}
	repo, env, hasEnv := strings.Cut(rest, "@")
	if hasEnv && env == "" {
		return Target{}, fmt.Errorf("invalid target %q: empty environment after @", s)
	}
	parts := strings.Split(repo, "/")
	if len(parts) < 2 || (t.Provider == GitHub && len(parts) != 2) {
		return Target{}, fmt.Errorf("invalid target %q: repository must be OWNER/REPO", s)
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\n@:") || strings.HasPrefix(p, "-") {
			return Target{}, fmt.Errorf("invalid target %q: bad repository path", s)
		}
	}
	if strings.ContainsAny(env, " \t\n@") || strings.HasPrefix(env, "-") {
		return Target{}, fmt.Errorf("invalid target %q: bad environment", s)
	}
	t.Repo, t.Env = repo, env
	return t, nil
}

func (t Target) String() string {
	s := string(t.Provider) + ":" + t.Repo
	if t.Env != "" {
		s += "@" + t.Env
	}
	return s
}

// runFunc runs name with args, feeding stdin, and returns combined output.
type runFunc func(ctx context.Context, name string, args []string, stdin string) ([]byte, error)

// Injectable for tests.
var (
	lookPath         = exec.LookPath
	run      runFunc = execRun
)

func execRun(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

func (t Target) cli() (string, error) {
	bin := "gh"
	hint := "install the GitHub CLI (https://cli.github.com) and run `gh auth login`"
	if t.Provider == GitLab {
		bin = "glab"
		hint = "install the GitLab CLI (https://gitlab.com/gitlab-org/cli) and run `glab auth login`"
	}
	if _, err := lookPath(bin); err != nil {
		return "", fmt.Errorf("%s not found on PATH: %s", bin, hint)
	}
	return bin, nil
}

// Push stores value as a CI secret named name at target. The value goes to the
// CLI on stdin, never argv.
func Push(ctx context.Context, target Target, name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	bin, err := target.cli()
	if err != nil {
		return err
	}
	var args []string
	switch target.Provider {
	case GitHub:
		args = []string{"secret", "set", name, "-R", target.Repo}
		if target.Env != "" {
			args = append(args, "--env", target.Env)
		}
	case GitLab:
		args = []string{"variable", "set", name, "-R", target.Repo, "--masked", "--protected"}
		if target.Env != "" {
			args = append(args, "--scope", target.Env)
		}
	default:
		return fmt.Errorf("unsupported provider %q", target.Provider)
	}
	return runCLI(ctx, bin, args, value, target, name, value)
}

// PushVariable stores a non-secret value as a CI variable. The value appears
// in argv, so use Push for anything sensitive.
func PushVariable(ctx context.Context, target Target, name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	bin, err := target.cli()
	if err != nil {
		return err
	}
	var args []string
	switch target.Provider {
	case GitHub:
		args = []string{"variable", "set", name, "--body", value, "-R", target.Repo}
		if target.Env != "" {
			args = append(args, "--env", target.Env)
		}
	case GitLab:
		args = []string{"variable", "set", name, value, "-R", target.Repo}
		if target.Env != "" {
			args = append(args, "--scope", target.Env)
		}
	default:
		return fmt.Errorf("unsupported provider %q", target.Provider)
	}
	return runCLI(ctx, bin, args, "", target, name, value)
}

func runCLI(ctx context.Context, bin string, args []string, stdin string, target Target, name, value string) error {
	out, err := run(ctx, bin, args, stdin)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if value != "" {
			msg = strings.ReplaceAll(msg, value, "***")
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New("set " + name + " on " + target.String() + " failed: " + bin + ": " + msg)
	}
	return nil
}

func checkName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return fmt.Errorf("invalid name %q: use letters, digits and underscores", name)
		}
	}
	return nil
}
