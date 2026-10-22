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

**Migration Cutover**:
The one-shot move of a live App from another platform to a Destination, declared in its `migrate` block: stop the old containers, prove the claimed data is free, deploy, repoint tunnel ingress, recorded in a journal for `yoho rollback --cutover`.
_Avoid_: Cutover (alone; that is also the Proxy switch inside every deploy), migration (alone; database migrations)

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

### Access

**Operator**:
Whoever runs Yoho with full SSH to a Server: every command, including setup and `yoho access`.
_Avoid_: Admin, owner, root user

**Member**:
A teammate or CI job given scoped access to one App Destination: fixed verbs through the Gate, no shell, no Docker. Yoho never hands a Member secret values, but code a Member deploys runs with the App's secrets.
_Avoid_: User, collaborator, deployer

**Gate**:
`yoho gate`, run by sshd from a Member key's forced command; it reads one request, checks the Member's verbs and the deploy policy, runs it, audits it and exits.
_Avoid_: Daemon, agent, API server, proxy

**Access Token**:
A `yoho_token_…` secret that CI exchanges for a short-lived Gate certificate; stored on the Server only as a hash.
_Avoid_: API key, deploy key, password

**Access Service**:
The opt-in, resident `yoho access serve` that signs short-lived SSH certificates for Members; it can only mint Gate certificates.
_Avoid_: Auth server, login server, CA
