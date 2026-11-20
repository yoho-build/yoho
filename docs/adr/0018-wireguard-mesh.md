# Opt-in WireGuard mesh

Status: Accepted

`network.mesh: wireguard` opts all Servers in the Yoho file into one private
IPv4 network. The kernel WireGuard implementation and wg-quick systemd unit
are operating-system facilities, not a resident Yoho daemon (ADR 0003).

The first Server by name with a public endpoint is the hub. Servers marked
`endpoint: none` initiate connections to it with a 25-second keepalive and
route the mesh subnet through it. Public Servers connect directly and route
NATed peers through the hub. Only the hub enables IPv4 forwarding. There is
no coordinator, NAT hole punching or automatic hub failover.

Allocations live in `<root>/mesh/state.json` on every Server. Read every copy
before changing anything; merge their allocation tables, using the first
Server in sorted name order as authority for conflicts. Preserve allocations
for removed Servers (tombstones) so addresses are never accidentally reused.
Reject duplicate addresses and subnet changes that invalidate allocations.
The operator must serialize mesh commands; partial updates converge on rerun.

Before installation, check routes (except the owned yoho0 interface and default
route) and all existing Docker network subnets for overlap. Refuse collisions.
Keys are generated on each Server with umask 077; only public keys return over
SSH. The root-owned configuration uses wg-quick's PostUp to load the private
key from its root-only file. Status filters the interface dump on the Server
before stdout, because its first row contains the private key.

Yoho owns yoho0, its unit, its WireGuard key/config, its mesh state and the
`yoho-mesh` UFW and iptables forwarding rules. Reconciliation deletes only
tagged rules; teardown preserves unrelated interfaces, networks and firewall policy. NATed Servers
need a known public egress IP for a restrictive hub firewall. Resolve the SSH
hostname of an `endpoint: none` Server for that allowlist; it must resolve to
its public egress IP (including dynamic DNS where necessary). Yoho cannot
infer arbitrary NAT egress addresses without a discovery service. Never add
an unrestricted UDP rule. Operators must serialize mesh operations.
