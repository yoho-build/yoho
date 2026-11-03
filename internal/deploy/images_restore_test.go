package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

const restoreRef = "yoho/shop-production-web:v1"

// tagHost answers `docker image inspect` of the Version tag from *tag and
// fails the compose `up` of the rollout when fail is set. docker tag
// commands move the tag, like the real daemon.
func tagHost(tag *string, fail *bool) *dockerHost {
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' '"+restoreRef+"'"):
			return *tag, nil
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' 'postgres:17'"):
			return "sha256:pg1", nil
		case strings.Contains(s, "docker tag 'sha256:aaaa1111' '"+restoreRef+"'"):
			*tag = "sha256:aaaa1111"
			return "", nil
		case *fail && strings.Contains(s, "'--scale'"):
			return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "unhealthy"}
		}
		return respond(nil)(s)
	}
	return h
}

// A deploy of a Version that fails after Build/Ship moved its tag puts the
// tag back on the image the deployed Release records, so a later rotate or
// rollback still finds it; prune keeps that ID throughout.
func TestFailedDeployRestoresVersionTag(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	tag, fail := "sha256:aaaa1111", false
	h := tagHost(&tag, &fail)
	if _, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil))); err != nil {
		t.Fatal(err)
	}

	tag, fail = "sha256:bbbb2222", true // the new build took the tag; its rollout fails
	h.scripts = nil
	if _, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil))); err == nil {
		t.Fatal("deploy must fail")
	}
	if tag != "sha256:aaaa1111" {
		t.Fatalf("tag = %s after the failed deploy, want the recorded image", tag)
	}
	if keep, err := RetainedImageRefs(ctx, h, "shop"); err != nil || !keep["sha256:aaaa1111"] {
		t.Fatalf("prune keep set %v %v", keep, err)
	}
}

// A successful deploy leaves the new tag alone.
func TestSuccessfulDeployKeepsNewTag(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	tag, fail := "sha256:aaaa1111", false
	h := tagHost(&tag, &fail)
	if _, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil))); err != nil {
		t.Fatal(err)
	}
	tag = "sha256:bbbb2222"
	if _, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.all(), "docker tag 'sha256:aaaa1111'") {
		t.Fatal("tag restored although the deploy succeeded")
	}
}

// The restore also runs when ctx is cancelled (Ctrl-C).
func TestRestoreVersionImagesSurvivesCancel(t *testing.T) {
	withRoot(t)
	tag, fail := "sha256:aaaa1111", false
	h := tagHost(&tag, &fail)
	if _, err := (Compose{}).Deploy(context.Background(), builtWeb(t, testDeploy(t, h, nil))); err != nil {
		t.Fatal(err)
	}
	tag = "sha256:bbbb2222"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	RestoreVersionImages(ctx, h, &bytes.Buffer{}, "s1", "shop", "production", "v1")
	if tag != "sha256:aaaa1111" {
		t.Fatalf("tag = %s", tag)
	}
}

// secrets rotate restarts from the deployed Release: a moved tag is
// re-pointed at the recorded image ID, and only a truly missing image fails.
func TestRotateRetagsFromRecordedID(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	tag, fail := "sha256:aaaa1111", false
	h := tagHost(&tag, &fail)
	first, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tag = "sha256:bbbb2222"
	d := builtWeb(t, testDeploy(t, h, nil))
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-secret-value-ABC"
	d.Restart, d.RestartFrom = []string{"web"}, first
	h.scripts = nil
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if retag, up := indexOf(t, h.scripts, "docker tag 'sha256:aaaa1111' '"+restoreRef+"'"), indexOf(t, h.scripts, "'--scale' 'web=2'"); retag > up {
		t.Error("tag must be restored before the restart")
	}

	tag = "sha256:bbbb2222"
	gone := tagHostGone(&tag)
	d.RestartFrom = first
	d.Servers[0].Host = gone
	_, err = (Compose{}).Deploy(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "cannot restore image "+restoreRef) || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v", err)
	}
}

func tagHostGone(tag *string) *dockerHost {
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' '"+restoreRef+"'"):
			return *tag, nil
		case strings.Contains(s, "docker tag"):
			return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "image gone"}
		}
		return respond(nil)(s)
	}
	return h
}
