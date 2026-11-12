package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type previewWorkflow struct {
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			With map[string]string `yaml:"with"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readPreviewWorkflow(t *testing.T) previewWorkflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "yoho-preview.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w previewWorkflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPreviewWorkflowUsesHeadForTeardown(t *testing.T) {
	w := readPreviewWorkflow(t)
	job := w.Jobs["preview"]
	if !strings.Contains(job.If, "github.event.pull_request.head.repo.full_name == github.repository") {
		t.Fatal("fork guard missing")
	}
	if !strings.Contains(job.If, "github.actor != 'dependabot[bot]'") {
		t.Fatal("Dependabot guard missing")
	}
	for _, s := range job.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			if s.With["ref"] != "${{ github.event.pull_request.head.sha }}" {
				t.Fatalf("closed PR would use different configuration: %s", s.With["ref"])
			}
			return
		}
	}
	t.Fatal("missing checkout step")
}

func TestPreviewWorkflowRequiresSSHCredentialsBeforeWriting(t *testing.T) {
	w := readPreviewWorkflow(t)
	var script string
	for _, s := range w.Jobs["preview"].Steps {
		if s.Name == "Configure SSH" {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatal("missing SSH step")
	}
	if strings.Contains(script, "accept-new") {
		t.Fatal("untrusted host-key policy")
	}
	for _, tc := range []struct{ name, key, known, want string }{
		{"no key", "", "host ssh-ed25519 key", "SSH_KEY"},
		{"no known hosts", "key", "", "ssh-known-hosts"},
		{"blank known hosts", "key", " \n\t", "ssh-known-hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home, "PREVIEW_SSH_KEY="+tc.key, "PREVIEW_KNOWN_HOSTS="+tc.known)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("missing credentials accepted or unclear failure: %v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
				t.Fatal("SSH files written before validation")
			}
		})
	}
}
