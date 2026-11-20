package transport

import (
	"context"
	"fmt"
	"io"

	"github.com/yoho-build/yoho/internal/remote"
)

// Decision describes a transport choice without including command output,
// which can contain credentials. Reason and Hint are safe to publish.
type Decision struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
	Hint   string `json:"hint"`
}

// Fallback returns a stable, actionable explanation for each auto fallback.
func Fallback(reason string) Decision {
	d := Decision{Mode: MethodLoad}
	switch reason {
	case "plugin":
		d.Reason = "pussh unavailable: docker pussh plugin not installed"
		d.Hint = "install docker pussh locally; run yoho setup to validate it"
	case "target":
		d.Reason = "pussh unavailable: no SSH target"
		d.Hint = "configure a Server SSH target or use transport.mode: load"
	case "probe":
		d.Reason = "pussh unavailable: cannot inspect Server Docker image store"
		d.Hint = "check Docker access on the Server"
	case "store":
		d.Reason = "pussh unavailable: Server lacks containerd image store"
		d.Hint = "run yoho setup --containerd-image-store (restarts Docker)"
	case "pussh":
		d.Reason = "pussh failed"
		d.Hint = "check docker pussh and SSH access; retry with transport.mode: pussh for diagnostics"
	case "load":
		d.Mode = MethodRegistry
		d.Reason = "docker load failed"
		d.Hint = "check Server disk space and Docker access; using the configured registry"
	case "container":
		d.Reason = "Apple container uses a full archive: docker load cannot safely reuse omitted OCI blobs"
		d.Hint = "use builder.engine: docker with docker pussh for layer-diff transfers"
	case "gate":
		d.Reason = "Gate uses a full archive: its validated docker load protocol requires every blob"
		d.Hint = "configure transport.mode: registry to reuse layers through the registry"
	default:
		panic("unknown transport fallback: " + reason)
	}
	return d
}

// MissingBlobs compares OCI content digests, never RootFS diff IDs (which
// identify uncompressed content and cannot establish compressed blob presence).
// Preserve archive order and send each missing blob once. Callers must also
// establish that their importer supports partial layouts before omitting blobs.
func MissingBlobs(required, present []string) []string {
	have := make(map[string]bool, len(present))
	for _, d := range present {
		have[d] = true
	}
	var missing []string
	for _, d := range required {
		if !have[d] {
			missing = append(missing, d)
			have[d] = true
		}
	}
	return missing
}

// Message formats a decision for human output, omitting an absent hint.
func (d Decision) Message(host string) string {
	message := fmt.Sprintf("%s: %s: %s", host, d.Mode, d.Reason)
	if d.Hint != "" {
		message += "; hint: " + d.Hint
	}
	return message
}

func validateMode(mode, engine string) error {
	switch mode {
	case "", "auto", MethodPussh, MethodLoad, MethodRegistry:
	default:
		return fmt.Errorf("unknown transport mode %q", mode)
	}
	return ValidateEngineMode(engine, mode)
}

func (p *pusher) report(h remote.Host, d Decision) {
	if p.o.OnDecision != nil {
		p.o.OnDecision(h.Name(), d)
		return
	}
	fmt.Fprintln(p.out, d.Message(h.Name()))
}

// Preview reports the transport expected before building. Auto may still fall
// back if pussh fails at transfer time. It never exports or ships an image.
func Preview(ctx context.Context, o PushOptions) (map[string]Decision, error) {
	if err := validateMode(o.Mode, o.Engine); err != nil {
		return nil, err
	}
	if o.Exec == nil {
		o.Exec = osExec
	}
	p := &pusher{o: o, out: io.Discard}
	result := map[string]Decision{}
	for _, h := range o.Hosts {
		d := Decision{Mode: o.Mode, Reason: "explicit transport", Hint: ""}
		if o.Engine == EngineContainer {
			d = Fallback("container")
		} else if o.Mode == "" || o.Mode == "auto" {
			d = p.pusshDecision(ctx, h)
		}
		result[h.Name()] = d
	}
	return result, nil
}

func (p *pusher) pusshDecision(ctx context.Context, h remote.Host) Decision {
	p.pusshOnce.Do(func() {
		p.pusshOK = LocalPusshAvailable(ctx, p.o.Exec)
	})
	if !p.pusshOK {
		return Fallback("plugin")
	}
	if p.target(h) == "" {
		return Fallback("target")
	}
	ok, err := ServerContainerdImageStore(ctx, h)
	if err != nil {
		return Fallback("probe")
	}
	if !ok {
		return Fallback("store")
	}
	return Decision{Mode: MethodPussh, Reason: "local plugin and Server containerd image store available", Hint: ""}
}
