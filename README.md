# bms-monitor

A small Go service that polls a HINAESS / PACE-Topband battery BMS over RS485 and
records the time series to a local SQLite database, so a multi-week cell-balancing
run can be inspected after the fact.

Today the tool offers:

- `bms-monitor poll` — run a single poll and print the decoded sample (Slice 1).
- `bms-monitor serve` — poll on a fixed interval forever and persist every sample
  to SQLite, running unattended under systemd (Slice 2), while serving a live web
  dashboard over HTTP (Slice 3).

## Building

The project is pure Go (the SQLite driver is `modernc.org/sqlite`, so no C
toolchain is needed). Build for the local machine with:

```sh
go build -o bms-monitor ./cmd/monitor
```

Cross-compile for a Raspberry Pi without any cross-toolchain:

```sh
# 64-bit Pi (Pi 3/4/5 on a 64-bit OS)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
  -o dist/bms-monitor-arm64 ./cmd/monitor

# 32-bit Pi OS
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
  -o dist/bms-monitor-armv7 ./cmd/monitor
```

## Configuration

`serve` reads `/etc/bms-monitor/config.yaml` by default (`--config` to change it).
A copy of every key, with its default, lives in [`config.example.yaml`](config.example.yaml).
CLI flags override config values, which override the built-in defaults.

```sh
sudo install -d /etc/bms-monitor
sudo cp config.example.yaml /etc/bms-monitor/config.yaml
sudo $EDITOR /etc/bms-monitor/config.yaml
```

## Deploying under systemd

The service runs as **root**, so there is no `bms` user to create and no
`dialout` group management — root already has read/write access to `/dev/ttyUSB*`.
`StateDirectory=bms-monitor` makes systemd create `/var/lib/bms-monitor` (owned by
root) so the default database path works out of the box.

> **Ensure NTP is running on the Pi before enabling the service.** Every sample is
> stamped with the host clock as Unix epoch milliseconds (UTC), and that timestamp
> is the join key across all three tables — a wrong clock makes the whole history
> useless. Confirm with `timedatectl` that `System clock synchronized: yes` before
> starting `bms-monitor`.

```sh
# 1. Install the binary
sudo install -m 0755 dist/bms-monitor-arm64 /usr/local/bin/bms-monitor

# 2. Install the config (see above)

# 3. Install and enable the unit
sudo cp bms-monitor.service /etc/systemd/system/bms-monitor.service
sudo systemctl daemon-reload
sudo systemctl enable --now bms-monitor
```

`systemctl enable --now bms-monitor` starts polling immediately and on every boot.
The unit uses `Restart=on-failure` / `RestartSec=10`, so a crash (or `kill -9`)
brings the service back automatically.

## Operating

```sh
systemctl status bms-monitor          # is it running?
journalctl -fu bms-monitor            # follow the structured JSON logs
sqlite3 /var/lib/bms-monitor/samples.db 'SELECT COUNT(*) FROM samples'
```

Each successful poll logs an INFO line with `pack_mv`, `pack_ma`, `soc_pct`, and
the headline balancing metrics `cells_min_mv` / `cells_max_mv` / `cells_delta_mv`.

## Web dashboard

`serve` also runs an embedded HTTP server in the same process as the poll loop,
serving a single-page dashboard so a balancing run can be watched live in a
browser — no separate daemon, no internet access required (uPlot and all assets
are baked into the binary). It listens on `web.listen`, **default `:8080`**:

```sh
# On the Pi (or via an SSH tunnel), open:
http://<pi-host>:8080/
```

The page shows a live header row (pack voltage/current, SOC, and cell
min/max/delta in mV, refreshed every ~5 s) and four charts: 16 cell-voltage
lines with a 3.65 V threshold line, cell delta (`max − min`), and a dual-axis
pack voltage/current chart. Range buttons (`1h / 6h / 24h / 7d / all`) reload the
historic series; long ranges are downsampled server-side to keep responses fast.

Change the listen address in `config.yaml` (`web.listen: ":8080"`) or override it
per-run without editing YAML:

```sh
bms-monitor serve --listen 0.0.0.0:9090
```

A failed bind (e.g. the port is already in use) is fatal — `serve` exits non-zero
so systemd restarts it and surfaces the misconfiguration. The dashboard is
LAN-only: no TLS, no authentication, no CORS. Restrict access at the network
layer (firewall, VPN, or an SSH tunnel) rather than exposing it to the internet.

Read-only JSON endpoints back the page and double as operator probes:

| Endpoint | Purpose |
| --- | --- |
| `GET /api/latest` | most-recent pack + per-cell + temperature sample |
| `GET /api/range?from=&to=&fields=&raw=` | column-oriented history for charts |
| `GET /api/health` | `200` `{"status":"ok",...}` while polling, `503` `"stale"` if no sample within `2×` the poll interval |

```sh
curl -s http://localhost:8080/api/health        # liveness probe
curl -s 'http://localhost:8080/api/latest' | jq .
```

## Teardown

After the balancing window, archive the data and remove the service:

```sh
sudo systemctl disable --now bms-monitor
cp /var/lib/bms-monitor/samples.db ~/balancing-archive.db   # optional archive
sudo rm /usr/local/bin/bms-monitor /etc/bms-monitor/config.yaml \
        /etc/systemd/system/bms-monitor.service
sudo rm -rf /var/lib/bms-monitor
```
