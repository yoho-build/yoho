# Yoho

An open-source, compose-native CLI that builds, ships, runs, backs up, and provisions self-hosted apps on servers over SSH, replacing Kamal, ONCE, and Ansible.

## Language

### Deploying

**App**:
One Docker Compose project deployed as a unit, plus its deploy settings.
_Avoid_: Project, stack

**Service**:
One service inside an App's compose file, with the same meaning as in Docker Compose.
_Avoid_: Container, role

**Server**:
A machine reachable over SSH that runs Apps.
_Avoid_: Host, node, VPS, box

**Destination**:
A named environment of an App, such as production or staging, with its own Servers and settings.
_Avoid_: Environment, stage, target

**Role**:
A named subset of an App's Services placed on a subset of a Destination's Servers. Reserved for later; today a Destination has one Server running the whole App.
_Avoid_: Group, tier

**Stateful Service**:
A Service that owns persistent data and is pinned to exactly one Server.
_Avoid_: Accessory, database service, singleton

**Release**:
The record of one deploy of an App to a Server: image digests, compiled compose, and secrets generation.
_Avoid_: Version, deployment, build

**Builder**:
The machine that builds an App's images: the local machine, a dedicated build Server, or the Destination's Server itself.
_Avoid_: Build host, runner

**Proxy**:
The single kamal-proxy on a Server that routes requests by hostname to Services and switches them over without downtime.
_Avoid_: Ingress, load balancer, gateway

**Hook**:
A user script run at a named point of a command, aborting the command on failure.
_Avoid_: Callback, plugin

**Scheduled Job**:
A recurring task, such as a Backup, run on a Server by Yoho without the operator's machine.
_Avoid_: Cron, sidecar, background task

### Backups

**Backup**:
A point-in-time copy of an App's selected volumes and dumps, taken with the App quiesced or paused.
_Avoid_: Snapshot, dump

**Backup Target**:
A place Backups are stored, such as a local directory, S3, B2, SFTP, or an rclone remote.
_Avoid_: Destination, repository, bucket
