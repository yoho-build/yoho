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
			Env  map[string]string `yaml:"env"`
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

func TestPreviewWorkflowHasOptInPruneJob(t *testing.T) {
	w := readPreviewWorkflow(t)
	job, ok := w.Jobs["prune"]
	if !ok {
		t.Fatal("missing prune safety-net job")
	}
	if !strings.Contains(job.If, "prune-older-than") || !strings.Contains(job.If, "YOHO_PREVIEW_PRUNE_OLDER_THAN") {
		t.Fatalf("prune job must be opt-in: %s", job.If)
	}
	var foundPrune bool
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "preview prune") && strings.Contains(s.Run, "--older-than") && strings.Contains(s.Run, "--yes") {
			foundPrune = true
		}
	}
	if !foundPrune {
		t.Fatal("prune job does not run preview prune --older-than ... --yes")
	}
}

func TestPreviewWorkflowFeatureDetection(t *testing.T) {
	w := readPreviewWorkflow(t)
	var up, prune string
	for _, s := range w.Jobs["preview"].Steps {
		if s.Name == "Update preview" {
			up = s.Run
		}
	}
	for _, s := range w.Jobs["prune"].Steps {
		if s.Name == "Prune stale previews" {
			prune = s.Run
		}
		if s.Name == "Configure SSH" && !strings.Contains(s.Env["PREVIEW_SSH_KEY"], "|| secrets.PREVIEW_SSH_KEY") {
			t.Fatal("schedule SSH key fallback missing")
		}
	}
	if up == "" || prune == "" {
		t.Fatal("missing scripts")
	}
	for _, tc := range []struct {
		name, script, help, age, keep, want string
		supported, fail                     bool
	}{
		{name: "legacy up", script: up, want: "up\n--name\npr-12\n"},
		{name: "modern up", script: up, help: "--pr int --branch string", want: "up\n--name\npr-12\n--pr\n12\n--branch\nfeat/demo\n"},
		{name: "partial up", script: up, help: "--branch string", want: "up\n--name\npr-12\n--branch\nfeat/demo\n"},
		{name: "legacy prune", script: prune, age: "7d", want: "::notice::"},
		{name: "age prune", script: prune, supported: true, age: "7d", want: "prune\n--yes\n--older-than\n7d\n"},
		{name: "keep only", script: prune, supported: true, age: " \t", keep: "0", want: "prune\n--yes\n--keep\n0\n"},
		{name: "both filters", script: prune, supported: true, age: "7d", keep: "2", want: "prune\n--yes\n--older-than\n7d\n--keep\n2\n"},
		{name: "blank filters", script: prune, supported: true, age: " \n", keep: "\t", want: "::error::", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			stub := `#!/bin/sh
set -eu
if [ "${3:-}" = --help ]; then
  if [ "$2" = up ]; then printf '%s\n' "$UP_HELP"; exit 0; fi
  if [ "$PRUNE_SUPPORTED" = true ]; then exit 0; fi
  exit 1
fi
printf '%s\n' "$@"
`
			if err := os.WriteFile(filepath.Join(bin, "yoho"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", tc.script)
			supported := "false"
			if tc.supported {
				supported = "true"
			}
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "UP_HELP="+tc.help, "PRUNE_SUPPORTED="+supported, "PRUNE_OLDER_THAN="+tc.age, "PRUNE_KEEP="+tc.keep, "PREVIEW_NUMBER=12", "PREVIEW_BRANCH=feat/demo")
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail || !strings.Contains(string(out), tc.want) {
				t.Fatalf("err=%v output=%q want=%q", err, out, tc.want)
			}
		})
	}
}
