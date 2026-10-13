package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// CheckOptions selects a read-only integrity check. Restic checks repository
// structure (all snapshots); ReadData explicitly adds reading all data packs.
// Archive checks one owned Backup (ID, default latest) in private staging.
// Neither proves that application data can be restored or loads a dump.
type CheckOptions struct {
	ListOptions
	ID       string
	ReadData bool
	Lock     LockFunc
}

// CheckResult states the scope verified; it is not a successful restore.
type CheckResult struct {
	ID       string `json:"id,omitempty"`
	Method   string `json:"method"`
	Verified string `json:"verified"`
}

// Check never initializes, restores, prunes or changes a Backup Target or App
// data. The Destination lock serializes it with Yoho writers. Restic uses
// --no-lock, so coordinate other tools writing this repository yourself.
func Check(ctx context.Context, o CheckOptions) (res *CheckResult, err error) {
	values := []string{o.Password}
	for _, v := range o.TargetEnv {
		values = append(values, v)
	}
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		err = secrets.RedactError(err, values...)
	}()
	if o.Host == nil {
		return nil, errors.New("backup check: Host is required")
	}
	if !nameRe.MatchString(o.App) || !nameRe.MatchString(o.Destination) {
		return nil, errors.New("backup check: invalid App or Destination")
	}
	project := o.Project
	if project == "" {
		project = release.ProjectName(o.App, o.Destination)
	}
	t, err := newTarget(o.App, o.Destination, project, o.Target, o.Password, o.TargetEnv)
	if err != nil {
		return nil, err
	}
	if t.typ == "restic" && o.ID != "" {
		return nil, errors.New("backup check: --id is only supported for archive targets; restic checks repository structure")
	}
	if t.typ == "archive" && o.ReadData {
		return nil, errors.New("backup check: --read-data is only supported for restic targets")
	}
	if o.Lock != nil {
		unlock, lerr := o.Lock(ctx)
		if lerr != nil {
			return nil, fmt.Errorf("lock: %w", lerr)
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			err = errors.Join(err, unlock(cctx))
		}()
	}
	return t.check(ctx, o.Host, release.BackupDir(o.App, o.Destination), o.ID, o.ReadData)
}

// check is strictly non-mutating to the target. Archive staging is exclusively
// created and removed here; fetch must never be used on the restic path.
func (t *target) check(ctx context.Context, h remote.Host, base, id string, readData bool) (res *CheckResult, err error) {
	if t.typ == "restic" {
		pre := t.readOnlyResticPrelude()
		if err := t.checkOwnedResticSnapshots(ctx, h); err != nil {
			return nil, err
		}
		args := "r check"
		verified := "repository structure; data packs not read"
		if readData {
			args += " --read-data"
			verified = "repository structure and all data packs"
		}
		if err := h.Run(ctx, remote.Cmd{Script: pre + args + "\n", Env: t.env}); err != nil {
			return nil, fmt.Errorf("restic check: %w", err)
		}
		// External writers do not share this Destination lock. Recheck owned
		// availability after structural checking to catch observed pruning;
		// this remains a point-in-time observation, not repository-wide locking.
		if err := t.checkOwnedResticSnapshots(ctx, h); err != nil {
			return nil, fmt.Errorf("after restic repository check: %w", err)
		}
		return &CheckResult{Method: t.method(), Verified: verified}, nil
	}
	if id == "" || id == "latest" {
		entries, err := t.list(ctx, h, base)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, errors.New("archive check: no Backups for this App/Destination")
		}
		id = entries[0].ID
	}
	if !t.archiveRe().MatchString(id) {
		return nil, fmt.Errorf("archive check: Backup %q does not belong to %s", id, t.project)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	name := "check-" + hex.EncodeToString(nonce[:16])
	token := hex.EncodeToString(nonce[16:])
	dir := path.Join(base, name)
	// Register before creation: mkdir/marker may complete even when SSH loses
	// its reply. Cleanup deletes only a directory whose private marker proves
	// this call created it, never an existing collision or unproven directory.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := removeCheckStaging(cctx, h, dir, token); cerr != nil {
			err = errors.Join(err, fmt.Errorf("remove private check staging %s: %w", dir, cerr))
		}
	}()
	if err := createCheckStaging(ctx, h, base, dir, token); err != nil {
		return nil, fmt.Errorf("create check staging %s (creation may have completed; an unproven directory is never deleted): %w", dir, err)
	}
	data, err := t.fetchArchive(ctx, h, dir, id, true)
	if err != nil {
		return nil, err
	}
	// Extraction validates the outer archive's content. Explicitly test it as
	// well (including ZIP/7z CRCs and tar/gzip readability), without App mounts.
	var verify string
	if t.format == "tar.gz" {
		verify = "set -eu\ndocker run --rm --network none" + mountFlags([]string{dir + ":/s:ro"}) + " " + remote.Quote(HelperImage) + " tar -tzf " + remote.Quote("/s/"+id) + " >/dev/null\n"
	} else {
		verify = t.sevenZip([]string{dir + ":/s:ro"}, `"$z" t -bd`+t.passFlag()+" "+remote.Quote("/s/"+id)+" >/dev/null")
	}
	if err := h.Run(ctx, remote.Cmd{Script: verify, Env: t.env}); err != nil {
		return nil, fmt.Errorf("archive check: integrity of %s: %w", id, err)
	}
	mb, err := h.ReadFile(ctx, path.Join(data, "manifest.json"), false)
	if err != nil {
		return nil, fmt.Errorf("archive check: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("archive check: parse manifest: %w", err)
	}
	if err := t.checkManifest(ctx, h, data, m); err != nil {
		return nil, err
	}
	return &CheckResult{ID: id, Method: t.method(), Verified: "outer archive, owned manifest, referenced files and volume tar/gzip readability; dumps not loaded"}, nil
}

// checkOwnedResticSnapshots validates the filtered response as well as presence
// of this App's exact tags; malformed or unrelated rows never count as owned.
func (t *target) checkOwnedResticSnapshots(ctx context.Context, h remote.Host) error {
	out, err := h.Output(ctx, remote.Cmd{Script: t.readOnlyResticPrelude() + "r snapshots --json --tag " + remote.Quote(t.tags()) + "\n", Env: t.env})
	if err != nil {
		return fmt.Errorf("restic check: list owned Backups (repository must already exist): %w", err)
	}
	var entries []struct {
		ID      string   `json:"id"`
		ShortID string   `json:"short_id"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return fmt.Errorf("restic check: parse snapshots: %w", err)
	}
	owned := false
	for _, entry := range entries {
		id := entry.ID
		wantLen := 64
		if id == "" {
			id, wantLen = entry.ShortID, 8
		}
		if len(id) != wantLen {
			return errors.New("restic check: malformed snapshot ID")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return errors.New("restic check: malformed snapshot ID")
		}
		tags := map[string]bool{}
		for _, tag := range entry.Tags {
			tags[tag] = true
		}
		if tags["yoho"] && tags["app="+t.app] && tags["destination="+t.dest] {
			owned = true
		}
	}
	if !owned {
		return errors.New("restic check: no Backups for this App/Destination; repository is empty or contains only unrelated Backups")
	}
	return nil
}

const checkOwnerMarker = ".yoho-check-owner"

func createCheckStaging(ctx context.Context, h remote.Host, base, dir, token string) error {
	// mkdir must succeed before the marker is written; no mkdir -p on dir.
	return createPrivateStaging(ctx, h, base, dir, token)
}

func removeCheckStaging(ctx context.Context, h remote.Host, dir, token string) error {
	return removePrivateStaging(ctx, h, dir, token)
}

func (t *target) readOnlyResticPrelude() string {
	mounts := []string{}
	pre := "set -eu\n"
	if strings.HasPrefix(t.cfg.Repository, "/") {
		pre += safeDir(path.Clean(t.cfg.Repository), false, false) + "test -d " + remote.Quote(t.cfg.Repository) + "\n"
		mounts = append(mounts, t.cfg.Repository+":"+t.cfg.Repository+":ro")
	}
	return pre + "if command -v restic >/dev/null 2>&1; then r() { restic --no-lock --no-cache \"$@\"; }; else r() { docker run --rm" + t.envFlags() + mountFlags(mounts) + " " + remote.Quote(ResticImage) + " --no-lock --no-cache \"$@\"; }; fi\n"
}

func (t *target) checkManifest(ctx context.Context, h remote.Host, data string, m Manifest) error {
	if m.App != t.app || m.Destination != t.dest || m.Project != t.project {
		return errors.New("archive check: manifest belongs to another App, Destination or project")
	}
	if m.CreatedAt.IsZero() || len(m.Services) == 0 {
		return errors.New("archive check: manifest requires created_at and Services")
	}
	files := map[string]bool{}
	var script strings.Builder
	script.WriteString("set -eu\n")
	checkFile := func(file string, tar bool) error {
		if !nameRe.MatchString(file) || file == "." || file == ".." || file == "manifest.json" || files[file] {
			return fmt.Errorf("archive check: invalid or duplicate manifest file %q", file)
		}
		files[file] = true
		p := remote.Quote("/in/" + file)
		script.WriteString("test -f " + p + " && test ! -L " + p + " && test -r " + p + "\n")
		if tar {
			script.WriteString("tar -tzf " + p + " >/dev/null\n")
		}
		return nil
	}
	for _, svc := range sortedKeys(m.Services) {
		ms := m.Services[svc]
		if !nameRe.MatchString(svc) || (len(ms.Volumes) == 0 && ms.Dump == "") {
			return fmt.Errorf("archive check: invalid or empty Service %q", svc)
		}
		for _, v := range sortedKeys(ms.Volumes) {
			if !nameRe.MatchString(v) {
				return fmt.Errorf("archive check: invalid volume %q", v)
			}
			if err := checkFile(ms.Volumes[v], true); err != nil {
				return err
			}
		}
		if ms.Dump != "" {
			if err := checkFile(ms.Dump, false); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	for _, n := range m.GeneratedSecrets {
		if !envNameRe.MatchString(n) || seen[n] {
			return errors.New("archive check: invalid or duplicate generated secret name")
		}
		seen[n] = true
		p := remote.Quote("/in/yoho-generated/" + n)
		script.WriteString("test -d /in/yoho-generated && test ! -L /in/yoho-generated\ntest -f " + p + " && test ! -L " + p + " && test -r " + p + "\n")
	}
	cmd := "set -eu\ndocker run --rm --network none" + mountFlags([]string{data + ":/in:ro"}) + " --entrypoint sh " + remote.Quote(HelperImage) + " -c " + remote.Quote(script.String())
	if err := h.Run(ctx, remote.Cmd{Script: cmd}); err != nil {
		return fmt.Errorf("archive check: referenced files or volume archives unreadable: %w", err)
	}
	return nil
}
