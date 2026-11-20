# Roles safety and operational routing

Status: Accepted. Supersedes the firewall and rollout details of ADR 0017.

Connection protection lives in mangle PREROUTING, before Docker DNAT and any
filter FORWARD ACCEPT rules. One tagged jump enters YOHO-ROLES; each App and
Destination has its own versioned YOHO-R-<hash> sub-chain. New original-direction
TCP traffic to the published private address/port RETURNs for declared consumers
and DROPs otherwise. Replies are never matched. Reconciliation fills a new
sub-chain and atomically replaces only that App's jump. Checks compare relative
rule content, so Apps never reorder each other's policy. The persistent oneshot
runs before Docker/network setup, replays every policy despite individual failures,
and reports a failure after the complete pass.

Roles is operator-only, including environment-selected scoped access. All
Destination-wide mutations coordinate every affected Server with one sorted-lock
helper; Service operations route to placement. Cross-Server depends_on requires
a declared connection. All Servers pass read-only preflight before mutation.
Once-only release commands form a separate dependency-ordered phase before any
replica of their Service switches, on the designated Server.

Generated-secret ownership is pinned in Release metadata and an App-owned
catalog at first generation, so an interrupted first rollout cannot lose the pin.
It cannot shift on placement edits. Only declared names reach Services; unused
consumer copies are pruned. Mesh allocations must already exist (yoho mesh up)
and addresses are refreshed after reconciliation.

Multi-Server ingress requires a named Tunnel or explicit Proxy ssl: false.
TLS is never silently rewritten. Tunnel ingress does not publish public 80/443;
custom-domain settings retain their TLS metadata for rollback.

A Service move never reruns release commands, verifies deployed Version/config
and target images, and retains the authoritative target on a late consumer
failure. Such a failure is partial, with durable step state; it must never restart
a stale source after target writes can have committed. Source cleanup is explicit.
Destinations without Roles preserve existing command behavior and lock metadata.
