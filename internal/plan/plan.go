// Package plan is the contract between the CLI orchestrator and the
// runtimes (compose, swarm). The CLI loads config, compose, secrets, builds
// and ships images, then hands a Deploy to a Runtime.
package plan

import (
	"context"
	"io"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/release"
	"github.com/yoho-dev/yoho/internal/remote"
)

// HookFunc runs the named Hook (pre-deploy, post-deploy, ...) with extra env.
// A nil HookFunc means no Hooks.
type HookFunc func(ctx context.Context, name string, env map[string]string) error

// Deploy is everything a Runtime needs to deploy one App to one Destination.
type Deploy struct {
	App         string
	Destination string
	Version     string
	Performer   string

	// Servers of the Destination, same order as config. compose: exactly one.
	// swarm: first is the manager.
	Servers []NamedHost

	// Loaded compose project. Service images are already set to the built or
	// pulled references for this Version; `build:` sections are removed.
	Project *types.Project
	// x-yoho per Service name.
	Ext map[string]config.ServiceExt

	// Resolved secret values per Service: Service -> container name -> value.
	// Only what each Service declared in x-yoho.secrets.
	ServiceSecrets map[string]map[string]string
	// Secret name -> provider reference (for audit), when known.
	SecretRefs map[string]string

	// Non-secret interpolation env for compose (Destination.env).
	Env map[string]string

	// Registry, when configured (swarm: --with-registry-auth).
	Registry       *config.Registry
	Proxy          config.ProxyConfig
	RetainReleases int
	Hook           HookFunc

	// Out receives progress output. Already wrapped by the secrets redactor.
	Out io.Writer
}

// NamedHost pairs a Server's config with an open Host.
type NamedHost struct {
	Name   string
	Server config.Server
	Host   remote.Host
}

// Runtime deploys and rolls back Apps on Servers.
type Runtime interface {
	// Deploy performs a zero-downtime deploy and returns the Release record
	// written on the Server(s).
	Deploy(ctx context.Context, d *Deploy) (*release.Release, error)
	// Rollback redeploys a previous Release (by version) without rebuilding.
	Rollback(ctx context.Context, d *Deploy, version string) (*release.Release, error)
	// Releases lists Releases on the (first) Server, newest first.
	Releases(ctx context.Context, d *Deploy) ([]release.Release, error)
}

// Action is what apply would do to one resource (Terraform-style).
type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"  // in place, zero-downtime (proxied) or no restart
	ActionReplace Action = "replace" // stop then start (Stateful / non-proxied Services)
	ActionDelete  Action = "delete"
	ActionNoop    Action = "noop"
)

// Change is one planned change.
type Change struct {
	// Kind: service, route, proxy, tunnel, job, secret, network.
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Server  string   `json:"server,omitempty"`
	Action  Action   `json:"action"`
	Reasons []string `json:"reasons,omitempty"` // e.g. "image yoho/web:f4e6 → d459", "secrets changed"
	// Downtime is true when apply interrupts this resource (replace of a non-proxied Service).
	Downtime bool `json:"downtime,omitempty"`
}

// Planner computes changes without modifying the Server (read-only).
// Runtimes implement it; images are not built, Deploy.Project images are the
// refs the Version would produce.
type Planner interface {
	Diff(ctx context.Context, d *Deploy) ([]Change, error)
}
