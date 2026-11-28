# WireGuard mesh

Yoho can manage an opt-in private network across all Servers in one Yoho file,
including Servers in different Destinations. It installs WireGuard tools on
Debian/Ubuntu using root or passwordless sudo. No Yoho daemon stays running.

```yaml
network:
  mesh: wireguard       # default: none
  subnet: 10.210.0.0/24 # default; private IPv4, checked against routes and Docker
  port: 51820          # default UDP listen port
servers:
  app1: {ssh: deploy@203.0.113.1, sudo: true}
  home: {ssh: deploy@home.example.com, sudo: true, endpoint: none}
```

`endpoint` defaults to the SSH hostname and mesh port. Set an explicit
`host[:port]` when the public WireGuard address differs. A public Server's
endpoint must accept UDP; its optional port is the externally reachable port,
while `network.port` is the local listen port. SSH aliases must resolve in DNS
or use an explicit endpoint. An `endpoint: none` Server's SSH hostname must
resolve to its public NAT egress IP for the peer firewall allowlist. Configure
public egress DNS first; Yoho has no public address discovery service.

Run `yoho mesh up` (also called by `yoho setup` after confirmation and by
`yoho apply`/`yoho deploy` for real Destinations; previews reuse the mesh).
It reconciles every configured Server, even when
`--destination` selects only one Destination. Serialize these commands across
Apps sharing a mesh. `yoho plan` reports interface changes, peer additions and
removals, and address assignments without installing anything.
`yoho plan` works before the first `yoho mesh up`. On a Roles Destination it
shows Connection addresses from the planned allocation and says they are
planned. `yoho apply` and `yoho deploy` bring the mesh up first, then read the
real allocation. They never fall back to `private_address`.

The first public Server in sorted name order is the hub. NATed Servers initiate
outbound connections to it every 25 seconds and route all mesh traffic through
it. Other public Servers connect directly and reach NATed Servers through the
hub. IPv4 forwarding and a UFW forwarding rule on `yoho0` are enabled on the
hub, along with an idempotent `yoho-mesh` iptables forwarding rule in
`DOCKER-USER` (or `FORWARD` when absent), persisted through wg-quick hooks.
Only directly connected peer public IPs receive inbound UDP rules tagged
`yoho-mesh`. Outbound-only Servers have no fixed listen port or inbound rule.
IPv6 UFW rules are added only when that Server enables IPv6 in UFW.
Yoho does not enable UFW or change its global policy; if UFW is disabled,
configure equivalent restrictions in the provider firewall. No NAT hole
punching, hub failover, or Swarm control-port firewall configuration is provided.

Stable addresses and public peer information are persisted in
`<root>/mesh/state.json` on every Server. Tables are merged before allocation;
the first Server by name is authoritative for conflicts. Removed Servers keep
address tombstones. A duplicate address or an allocation outside the configured
subnet is refused. Rerun after a partial failure to reconcile.

Private keys are generated on their Server in root-only
`/etc/wireguard/yoho0.key`. They never return over SSH, appear in local files,
or enter logs. The root-owned `0600` wg-quick configuration loads the key with
`PostUp`. `wg-quick@yoho0` is enabled for reboot persistence.

```sh
yoho mesh status --json # peer handshake age (never if absent), rx/tx bytes, ping RTT
yoho mesh rotate-keys   # regenerate keys on Servers and rewrite all peer lists
yoho mesh down          # remove owned yoho0 files, keys, unit enablement and UFW rules
```

Rotation and interface changes can briefly interrupt mesh traffic. Teardown
removes Yoho's forwarding sysctl file, but leaves the current kernel forwarding
setting intact because Docker or another network may need it. Teardown leaves
unrelated networks and firewall rules intact. To remove a Server, remove it
from the config and run
`yoho mesh up`. Surviving Servers remove its peer while retaining its address
tombstone and all existing allocations. Keep SSH access to the removed Server
and tear down its mesh separately with a temporary Yoho file listing only that
Server and its original `root` and SSH routing. `yoho mesh down` is explicit full
teardown of every Server listed in its config and deletes their allocation state;
never run it on surviving Servers as part of Server removal.

Swarm uses the assigned mesh address for `--advertise-addr`; run `mesh up`
before `swarm init` or `swarm join`. Roles Connections use each Server's mesh
address in place of `private_address`, and the Connection firewall matches
traffic on those addresses. An [instance connection](roles.md#instance-connections)
points `<server>.m.<service>` at that Server's mesh address, so a consumer on a
NATed Server reaches every Instance through the hub.

| Support | Behavior |
| --- | --- |
| Linux Debian/Ubuntu + systemd | WireGuard tools, wg-quick, UFW reconciliation |
| Public Servers | Direct peers; first by name is hub |
| NATed Servers (`endpoint: none`) | Outbound hub connection; keepalive 25 seconds |
| Other operating systems / full NAT traversal | Not supported |
