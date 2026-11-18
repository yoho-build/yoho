package deploy

import (
	"reflect"
	"strings"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

// GateHardeningReason prefixes the plan reason for compose keys whose only
// difference from the current Release is what the Gate added when it
// deployed it (gate.Policy.Harden: cap_drop NET_RAW, pids_limit,
// pull_policy never, mem_limit, cpus, deploy.resources.limits, tmpfs
// sizes). An operator deploy compiles without them, so it really
// recreates those Services; the reason says why.
const GateHardeningReason = "Gate hardening removed by an operator deploy: "

// GateHardeningWarning is the warning for an operator plan with
// GateHardeningReason changes, or "".
func GateHardeningWarning(changes []plan.Change) string {
	for _, c := range changes {
		for _, r := range c.Reasons {
			if strings.HasPrefix(r, GateHardeningReason) {
				return "the current Release was deployed through the Gate; an operator deploy recreates these Services without the Gate limits. Run `YOHO_ACCESS=scoped yoho plan` to see what a Gate deploy changes"
			}
		}
	}
	return ""
}

// gateDeployed reports whether rel was deployed (or rolled back) through
// the Gate: its performer is "<member> (gate)[ <client>]".
func gateDeployed(rel *release.Release) bool {
	return rel != nil && strings.Contains(rel.Performer, " (gate)")
}

// splitHardening splits changed keys of one Service into those whose only
// difference is what Harden added to old (the Gate-deployed entry) and the
// rest.
func splitHardening(keys []string, old, want map[string]any) (hardened, other []string) {
	for _, k := range keys {
		if hardenedOnly(k, old[k], want[k]) {
			hardened = append(hardened, k)
		} else {
			other = append(other, k)
		}
	}
	return hardened, other
}

// hardenedOnly reports whether old, the value of key k in a Gate-hardened
// container spec, is want with only the Gate's hardening added.
func hardenedOnly(k string, old, want any) bool {
	switch k {
	case "cap_drop":
		o, w := strList(old), strList(want)
		if len(o) != len(w)+1 || !strings.EqualFold(o[len(o)-1], "NET_RAW") {
			return false
		}
		return reflect.DeepEqual(o[:len(w)], w)
	case "pull_policy":
		return old == "never" && want != "never"
	case "pids_limit", "mem_limit", "cpus":
		// Harden only fills these in when the Service sets none.
		return old != nil && want == nil
	case "deploy":
		return hardenedDeploy(old, want)
	case "tmpfs":
		return hardenedList(old, want, func(o, w any) bool {
			ov, ok1 := o.(string)
			wv, ok2 := w.(string)
			if !ok1 || !ok2 || !strings.HasPrefix(ov, wv) {
				return false
			}
			rest := ov[len(wv):]
			return (strings.HasPrefix(rest, ":size=") || strings.HasPrefix(rest, ",size=")) && !strings.ContainsAny(rest[1:], ",:")
		})
	case "volumes":
		return hardenedList(old, want, hardenedTmpfsVolume)
	case "pre_start":
		return hardenedList(old, want, func(o, w any) bool {
			om, ok1 := o.(map[string]any)
			wm, ok2 := w.(map[string]any)
			if !ok1 || !ok2 {
				return false
			}
			for _, key := range changedKeys(om, wm) {
				if !hardenedOnly(key, om[key], wm[key]) {
					return false
				}
			}
			return true
		})
	}
	return false
}

func strList(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// hardenedList compares two lists element by element; differing elements
// must pass same.
func hardenedList(old, want any, same func(o, w any) bool) bool {
	o, ok1 := old.([]any)
	w, ok2 := want.([]any)
	if !ok1 || !ok2 || len(o) != len(w) {
		return false
	}
	for i := range o {
		if !reflect.DeepEqual(o[i], w[i]) && !same(o[i], w[i]) {
			return false
		}
	}
	return true
}

// hardenedTmpfsVolume: a tmpfs volume whose old entry only adds tmpfs.size.
func hardenedTmpfsVolume(o, w any) bool {
	om, ok1 := o.(map[string]any)
	wm, ok2 := w.(map[string]any)
	if !ok1 || !ok2 || om["type"] != "tmpfs" {
		return false
	}
	ot, _ := om["tmpfs"].(map[string]any)
	if ot == nil || ot["size"] == nil {
		return false
	}
	wt, _ := wm["tmpfs"].(map[string]any)
	if wt != nil && wt["size"] != nil {
		return false
	}
	return reflect.DeepEqual(without(om, "tmpfs", "size"), without(wm, "tmpfs", "size"))
}

// hardenedDeploy: the deploy sections differ only in resources.limits, and
// every limit want sets has the same value in old.
func hardenedDeploy(old, want any) bool {
	om, ok := old.(map[string]any)
	if !ok {
		return false
	}
	wm, _ := want.(map[string]any)
	if !reflect.DeepEqual(without(om, "resources", "limits"), without(wm, "resources", "limits")) {
		return false
	}
	ol := limitsOf(om)
	if ol == nil {
		return false
	}
	for k, v := range limitsOf(wm) {
		if !reflect.DeepEqual(ol[k], v) {
			return false
		}
	}
	return true
}

func limitsOf(m map[string]any) map[string]any {
	r, _ := m["resources"].(map[string]any)
	l, _ := r["limits"].(map[string]any)
	return l
}

// without returns m with m[outer][inner] removed (and m[outer] when that
// leaves it empty), never changing m. A nil or empty result is nil.
func without(m map[string]any, outer, inner string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	if sub, ok := out[outer].(map[string]any); ok {
		c := map[string]any{}
		for k, v := range sub {
			if k != inner {
				c[k] = v
			}
		}
		if len(c) == 0 {
			delete(out, outer)
		} else {
			out[outer] = c
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
