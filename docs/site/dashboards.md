# Dashboards

`yoho tui` (terminal) and `yoho ui` (browser) show the selected Destination at a glance:

- every Server: OS, kernel, uptime, load, memory, disk of the Yoho and Docker roots, Docker version;
- the App's containers with state, health, CPU, memory, network I/O, image and ports;
- the Proxy (`yoho-proxy`);
- Release history, with the current Release marked;
- the last lines of any listed container's logs, with a time range and a search run on the Server (`yoho ui`) or a filter as you type (`yoho tui`);
- on demand, the size of the App directory and each of the App's volumes.

```sh
yoho tui                     # q to quit
yoho ui                      # opens the browser; Ctrl-C to stop
yoho ui -d staging --port 7300 --no-open
```

Both run on your machine and read the Servers over SSH, like `yoho app ps`. Nothing is installed or left running on a Server, and nothing can be changed from them. Only Yoho-owned containers are listed: the App's compose project (or swarm stack) and `yoho-proxy`; other containers on the Server are never shown or read.

## Service details in `yoho tui`

Select a Service with `↑↓` and press `d` for Details: CPU percentage, memory usage
and limit, memory percentage, network I/O (received / transmitted), disk I/O
(read / written), and PIDs. The page also shows state, health, image, ports,
and container identity. Each replica has its own row and Details page.

Details refresh at the configured interval (5 seconds by default); press `r`
to refresh immediately. Network and disk I/O are cumulative counters, not
per-second rates. Stopped containers and unavailable stats show `—`.

Use `↑↓` or `pgup`/`pgdn` to scroll, `g`/`G` for the top/bottom, and `esc` or `q`
to return. `Enter` opens logs from either the overview or Details; leaving logs
returns to the page you opened them from.

## Volume disk usage

Press `v` in `yoho tui`, or **Show disk usage** on a Server in `yoho ui`, to size the App directory and each of the App's named volumes, with the largest files and directories at the top of each volume. Sizes come from `du` over SSH; nothing is installed. A scan walks every file, so it runs only when asked (`r` / **Rescan**) and is reused for 10 minutes. It runs at idle CPU and I/O priority, stops each volume after 120 s, and sizes no more volumes after 300 s. A size shown as `≥` is a lower bound because `du` hit an error or was stopped.

Volume data belongs to root, so volumes are sized only when the SSH user is root or may run `du` through passwordless sudo. Otherwise the volumes are listed without sizes, with a note. A rule for `du` alone is enough, and it doesn't need `servers.<name>.sudo: true`:

```sh
echo 'deploy ALL=(root) NOPASSWD: /usr/bin/du' | sudo tee /etc/sudoers.d/yoho-du
sudo chmod 440 /etc/sudoers.d/yoho-du && sudo visudo -c
```

Only plain local volumes are sized. A volume with driver options (NFS, CIFS, or a bind of another host directory) or another driver is listed as not sized. Volume sizes aren't available through scoped access yet.

## `yoho ui` access

The dashboard listens on `127.0.0.1` only. `yoho ui` prints a one-time login link and opens it unless `--no-open`. The link signs in that browser (new tabs on the same address stay signed in) and then stops working; to use another browser, restart `yoho ui`. Several `yoho ui` instances, one per Destination, can run side by side. Requests with a non-loopback `Host` header are refused, and the API answers only `GET`.

Containers that exited with code 0 (release commands, one-off jobs) are shown as completed, not as failures; Swarm's retained history of replaced tasks is hidden. On Swarm, Release history is read from the manager (the first Server).

To reach it from another machine, forward the port over SSH (`ssh -L 7300:127.0.0.1:7300 laptop`) rather than exposing it.

## Logs

The `yoho ui` logs dialog shows the last 100, 200 or 1000 lines, optionally of the last 15 minutes, hour, 6 hours or 24 hours only. The search box filters on the Server (`grep`, fixed string, ignoring case), so the line count is of matching lines and older matches are found too. In `yoho tui` logs, `/` filters the loaded lines as you type (ignoring case); `enter` keeps the filter, `esc` clears it. For more, use `yoho app logs` with `--since`, `--until`, `--grep` and `-f` (see the [CLI reference](cli.md)).

Container output is shown as Docker returns it. Yoho does not resolve your secrets for these views, so it cannot redact a value an application prints; keep secrets out of application logs.

## Metrics history and alerts

`yoho ui` and `yoho tui` show the present: nothing is recorded while they are closed, and nothing alerts you. Yoho runs no daemon on your Servers ([ADR 0003](https://github.com/yoho-build/yoho/blob/main/docs/adr/0003-no-resident-daemon-by-default.md)). For history or alerts, add a tool for it. Both recipes are **optional and outside Yoho**: you run and maintain them; Yoho neither installs nor checks them.

**Metrics history with Grafana Cloud (or any Prometheus).** Run [Grafana Alloy](https://grafana.com/docs/alloy/latest/) on each Server with its built-in node (`prometheus.exporter.unix`) and cAdvisor (`prometheus.exporter.cadvisor`) exporters, sending to your Grafana Cloud stack with `prometheus.remote_write`, and set alert rules in Grafana. Deploy it as its own Yoho App (the `grafana/alloy` image with `/proc`, `/sys`, `/` and `/var/run/docker.sock` mounted read-only, and the Grafana Cloud token as a Yoho secret) or with your usual tooling. Self-hosted Prometheus with `node_exporter` and cAdvisor works the same way.

**Uptime alerts with an external monitor and a heartbeat.** Point a monitor on another machine ([Uptime Kuma](https://github.com/louislam/uptime-kuma), [Better Stack](https://betterstack.com/uptime), Healthchecks.io) at your App's public URL for availability alerts. For the Server itself, a root crontab line such as `*/5 * * * * curl -fsS -m 10 https://uptime.betterstack.com/api/v1/heartbeat/<id> >/dev/null` (or an Uptime Kuma push monitor URL) pings a heartbeat; the monitor alerts when the pings stop.

An opt-in per-Server API for unattended metrics and alerting is on the [backlog](https://github.com/yoho-build/yoho/blob/main/docs/backlog.md) ("Remote API + dashboard").

## Developing the web app

The app lives in `web/` (Vite, React, Tailwind, shadcn components on Base UI) and is embedded into the binary from `internal/webui/dist`:

```sh
npm --prefix web ci
npm --prefix web run build          # writes internal/webui/dist; commit it
yoho ui --port 7300 --no-open        # in an App directory
npm --prefix web run dev             # hot reload; proxies /api and /login to :7300
```

With `npm run dev`, open the login link printed by `yoho ui` on the dev server's port instead (replace `7300` with Vite's port).
