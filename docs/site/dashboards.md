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

## `yoho ui` access

The dashboard listens on `127.0.0.1` only. `yoho ui` prints a one-time login link and opens it unless `--no-open`; the link sets a session cookie for that browser and then stops working. To use another browser, restart `yoho ui`. Requests with a non-loopback `Host` header are refused, and the API answers only `GET`.

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
