# ping-lens

Lightweight network monitoring for benchmarking one ISP, or comparing several.

`ping-lens` runs on small Linux devices (Raspberry Pi, mini-PCs, anything that
can speak ICMP). Each device continuously probes a set of targets, classifies
failure patterns, computes percentile rollups locally, and periodically ships
the data to a Postgres database (e.g. Neon). A built-in HTTP server renders a
read-only dashboard sourced from those rollups.

Two operating modes:

- **Single ISP benchmark** — one source. Answers "is my internet good?"
- **Multi-ISP comparison** — one source per ISP. Answers "which is better?"

The dashboard auto-detects the mode based on how many sources have data, or
you can force it via `mode: single_isp` / `mode: multi_isp` in the config.

## Quick start

```sh
# 1. Build
make build           # -> bin/ping-lens

# 2. Copy + edit config
cp config.example.yaml /etc/ping-lens/config.yaml
$EDITOR /etc/ping-lens/config.yaml

# 3. Provide the database URL
echo 'PING_LENS_DATABASE_URL=postgres://...' > /etc/ping-lens/ping-lens.env

# 4. Install systemd unit
sudo install -m 0644 deploy/ping-lens.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin ping-lens || true
sudo install -d -o ping-lens -g ping-lens /var/lib/ping-lens/spool
sudo systemctl daemon-reload
sudo systemctl enable --now ping-lens
```

The dashboard then lives at `http://<host>:8080/`.

## Architecture

```
                      probe loop (per target)
                              │
                              ▼
                      sample channel
                              │
                ┌─────────────┼──────────────┬──────────────────────┐
                ▼             ▼              ▼                      ▼
          NDJSON spool   rollup accum   outage tracker         (future)
          on disk        (in memory)    (in memory)
                              │              │
                              ▼              ▼
                       30-min flush ─▶ Postgres (Neon)
                                       (ping_samples,
                                        ping_rollups,
                                        outage_events)
                                              │
                                              ▼
                                       built-in dashboard
```

### Storage

- `ping_samples` — every raw probe; default retention 14 days.
- `ping_rollups` — 30-minute aggregates with p50/p90/p95/p99; 1-year retention.
- `outage_events` — classified outage start/end records.
- `loaded_latency_tests` — table reserved for post-MVP loaded-latency runs.

Schema lives in [`migrations/001_init.sql`](migrations/001_init.sql) and is
embedded in the binary; it is applied (idempotent) on every start.

### Reliability

- The probe loop never blocks on database writes. Samples are appended to a
  rotating NDJSON spool file in `flush.spool_dir`.
- Every `flush.interval_minutes` the spool is rotated and consumed via
  `COPY FROM`. Successful files are renamed to `*.done.ndjson` and removed
  after `retention.spool_hours`.
- If Postgres is unreachable, spool files accumulate and are retried on the
  next cycle. Rollups + outage events queue in memory until the flush
  succeeds.
- Outage classification follows the PRD rules:
  - one target down → `single_target_issue`
  - several targets down → `multi_target_issue`
  - all public targets down (LAN gateway up) → `full_isp_outage`
  - hostname DNS fails but IP probes work → `dns_issue`
  - LAN gateway fails → `local_gateway_issue`

### Grading

For a single source we report a worst-axis-wins quality label
(`excellent` / `good` / `fair` / `poor` / `bad`) computed over:

- packet-loss percentage
- p95 latency
- p99 latency
- outage count + longest outage

Thresholds are configurable; the built-in defaults match the PRD.

For multi-ISP mode the winner is chosen by counting leads across loss, p95,
p99, outage count, and per-target p95 leads. A `low`/`medium`/`high`
confidence label is attached based on sample volume, days monitored, and the
size of the lead.

## HTTP endpoints

| Path        | Description                                |
|-------------|--------------------------------------------|
| `/`         | Auto-mode dashboard (single or multi ISP)  |
| `/report`   | Evidence report for `?source=<name>`       |
| `/healthz`  | JSON liveness + config summary             |

Add `?window=1h|24h|7d|30d|evening_peak` to override the default 7-day view.

## ICMP privileges

Raw ICMP requires either root or `CAP_NET_RAW`. The provided systemd unit
grants the capability via `AmbientCapabilities`. If you cannot grant raw
sockets, set `probe.privileged: false` in the config and configure
`net.ipv4.ping_group_range` so unprivileged ICMP via UDP is allowed for the
service user.

## Development

```sh
make build          # produce bin/ping-lens
make test           # run unit tests
make vet            # static checks
make cross          # build linux/{arm64,amd64} binaries for Pi + servers
```

The dashboard templates are embedded via `//go:embed`; no external assets
beyond the Chart.js CDN script are required.

## What's not in the MVP

Per the PRD, these land later: loaded-latency tests, scheduled latency
testing, traceroute snapshots, TCP/HTTP probes, Prometheus metrics, alerting,
authentication, mobile-first layout, CSV export, and public report sharing.
