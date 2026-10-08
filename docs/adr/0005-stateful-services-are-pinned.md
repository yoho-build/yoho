# Stateful Services are pinned to one Server

A Service marked `x-yoho.stateful: true` runs on exactly one Server; deploying a Service with volumes to several Servers without that mark is an error, and moving a Stateful Service needs an explicit move onto an empty volume. Running the same compose on two Servers silently creates two databases; Kamal, Uncloud and Swarm leave this unguarded, only Coolify refuses. Config keeps `servers` as a list from v1 (validated to one) so Roles can be added without a breaking change.
