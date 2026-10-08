package cli

import (
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/schedule"
)

func TestJobDiffRuntime(t *testing.T) {
	j := &backupJob{cfg: config.BackupJob{Schedule: "daily"}}
	spec := schedule.Spec{Schedule: "daily"} // written before Runtime existed
	if r := jobDiff(spec, j, "compose"); len(r) != 0 {
		t.Errorf("old spec equals compose: %v", r)
	}
	if r := jobDiff(spec, j, ""); len(r) != 0 {
		t.Errorf("unset runtime equals compose: %v", r)
	}
	r := jobDiff(spec, j, "swarm")
	if len(r) != 1 || !strings.Contains(r[0], "runtime compose → swarm") {
		t.Errorf("got %v", r)
	}
}

func TestSecretDiffDetectsRotation(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	mk := func(pw, aws string) *backupJob {
		return &backupJob{
			target:   config.BackupTarget{PasswordSecret: "PW", EnvSecrets: []string{"AWS_SECRET_ACCESS_KEY"}},
			password: pw, env: map[string]string{"AWS_SECRET_ACCESS_KEY": aws},
		}
	}
	installed := schedule.Spec{SecretFingerprints: targetFingerprints(key, mk("pw", "aws1"))}
	if r := secretDiff(installed, targetFingerprints(key, mk("pw", "aws1"))); r != "" {
		t.Errorf("unchanged: %q", r)
	}
	r := secretDiff(installed, targetFingerprints(key, mk("pw", "aws2")))
	if r != "credential changed: AWS_SECRET_ACCESS_KEY" {
		t.Errorf("rotation: %q", r)
	}
	for _, v := range targetFingerprints(key, mk("pw", "aws1")) {
		if strings.Contains(v, "aws1") || len(v) != 16 {
			t.Errorf("fingerprint %q", v)
		}
	}
	if r := secretDiff(schedule.Spec{}, targetFingerprints(key, mk("pw", "aws1"))); r == "" {
		t.Error("spec without fingerprints must be refreshed once")
	}
	if r := secretDiff(schedule.Spec{}, nil); r != "" {
		t.Errorf("no secrets, no diff: %q", r)
	}
}
