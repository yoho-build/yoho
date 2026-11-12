# Preview Destinations

Previews are ephemeral Destinations derived at run time from `previews.from`,
with their own compose project, App directory, named volumes and Proxy routes.
They reuse the template's Yoho and compose overlays, Servers, interpolation and
secrets selection (`.yoho/secrets` plus `.yoho/secrets.<from>`, including provider
Destination filters). Hosts come from `previews.host`. The template is never
modified or persisted as another configured Destination.

Preview creation and teardown require operator access and the compose runtime.
They clear Migration Cutover, Backup and Scheduled Job settings, tunnel
management, `app_dir` and the fixed network subnet. External volumes, writable
bind mounts and fixed container names are refused; named volumes are scoped to
the preview even when the template names them explicitly. The template must use
preview-safe credentials and side effects. Preview data starts empty; seeding
from a Backup is future work.

The normal deploy lock covers preview builds, rollout and writing `preview.json`
in the App directory. This marker records the exact App, Destination, project,
hosts, Services and attempted Version before building, allowing failed first
deploys to be cleaned up. Updating a preview is idempotent. An unmarked existing
project is never adopted. Configured Destination name collisions are refused.

`yoho preview down` verifies the marker and project ownership under the same
Destination lock before deleting containers, routes, networks, volumes and the
App directory. The marker stays until cleanup finishes so interrupted teardown
can be retried; an absent preview is success. It reuses `remove`'s exact project
label selectors, including anonymous-volume deletion, rather than requiring a
current Compose file (a first deploy may have failed). Shared Proxy resources,
the `yoho` network, Backups and other projects stay.

This is the preview-only exception to the original never-remove-volumes rule
in ADR 0005 and VISION.md; generic `docker compose down -v` remains forbidden.
The existing explicit `yoho remove --volumes --confirm APP` behavior is retained.
Only `preview down` offers volume removal based on a preview marker; a configured
Destination can never be removed through it. There is no resident preview daemon
or automatic expiry (ADR 0003). Inventory and scheduled cleanup use `preview list`
and explicit `preview down` calls. Scoped Gate support and Backup seeding are
future work.
