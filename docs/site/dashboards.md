# Dashboards

`yoho tui` (terminal) and `yoho ui` (browser) show the selected Destination at a glance:

- every Server: OS, kernel, uptime, load, memory, disk of the Yoho and Docker roots, Docker version;
- the App's containers with state, health, CPU, memory, network I/O, image and ports;
- the Proxy (`yoho-proxy`);
- Release history, with the current Release marked;
- the last lines of any listed container's logs.

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

## `yoho ui` access

The dashboard listens on `127.0.0.1` only. `yoho ui` prints a one-time login link and opens it unless `--no-open`. The link signs in that browser (new tabs on the same address stay signed in) and then stops working; to use another browser, restart `yoho ui`. Several `yoho ui` instances, one per Destination, can run side by side. Requests with a non-loopback `Host` header are refused, and the API answers only `GET`.

Containers that exited with code 0 (release commands, one-off jobs) are shown as completed, not as failures; Swarm's retained history of replaced tasks is hidden. On Swarm, Release history is read from the manager (the first Server).

To reach it from another machine, forward the port over SSH (`ssh -L 7300:127.0.0.1:7300 laptop`) rather than exposing it.

## Logs

Container output is shown as Docker returns it. Yoho does not resolve your secrets for these views, so it cannot redact a value an application prints; keep secrets out of application logs.

## Developing the web app

The app lives in `web/` (Vite, React, Tailwind, shadcn components on Base UI) and is embedded into the binary from `internal/webui/dist`:

```sh
npm --prefix web ci
npm --prefix web run build          # writes internal/webui/dist; commit it
yoho ui --port 7300 --no-open        # in an App directory
npm --prefix web run dev             # hot reload; proxies /api and /login to :7300
```

With `npm run dev`, open the login link printed by `yoho ui` on the dev server's port instead (replace `7300` with Vite's port).
