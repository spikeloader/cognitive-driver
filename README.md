# cognitive-driver — observability micro-AI (MVP)

[![CI](https://github.com/spikeloader/cognitive-driver/actions/workflows/ci.yml/badge.svg)](https://github.com/spikeloader/cognitive-driver/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/spikeloader/cognitive-driver)](https://github.com/spikeloader/cognitive-driver/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/spikeloader/cognitive-driver)](https://goreportcard.com/report/github.com/spikeloader/cognitive-driver)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

[Português](README.pt-BR.md) · **English**

A lightweight Go daemon that reads Prometheus exporters (`node_exporter` and
service exporters), keeps an adaptive baseline per metric (EWMA mean and
exponential standard deviation), detects anomalies, explains each decision and
recommends an action. It runs strictly in **`observe_only`** mode: it never
executes commands on the host.

```
node_exporter → Collector → Feature Engine → Anomaly Engine → Explanation Engine → Policy Engine → API / SQLite / JSON logs
```

- **No rules to write for the common case**: each host × metric learns its own
  baseline; fixed rules exist only for unambiguous states (service down, pool
  exhausted, cron job failed).
- **Every event is explained**: what was observed, how far from the baseline,
  probable cause and a concrete next step.
- **Every event names a suspect**: CPU, memory and disk events show the
  process group that was consuming the most of that resource at that moment.
- **One binary, no external services**: SQLite for events and baselines, web
  UI embedded; about 22 MB RSS with the host and Docker drivers.
- **Drivers in YAML**: host, nginx, Apache, Docker, PHP-FPM, Swoole, cron
  jobs, Redis, Memcached, PostgreSQL and MySQL ship ready to use; any
  Prometheus exporter becomes a driver without code.

> **Languages:** the web UI and the event texts (explanation, probable cause,
> recommendation) are available in English and Brazilian Portuguese. Pick the
> language at the top of the UI; it defaults to the browser's. The API's main
> fields are in Portuguese and each event also carries `translations.en`.

## Installing on a host

Requirements: Linux with systemd (tested on Ubuntu 26.04), Go 1.27+ to build
from source, and `prometheus-node-exporter`, which the installer installs via
apt when nothing answers on `127.0.0.1:9100` (disable with
`--without-node-exporter`).

From a [release](https://github.com/spikeloader/cognitive-driver/releases)
(linux amd64/arm64, no Go required):

```bash
curl -LO https://github.com/spikeloader/cognitive-driver/releases/download/v0.1.0/cognitive-driver-0.1.0-linux-amd64.tar.gz
tar -xzf cognitive-driver-0.1.0-linux-amd64.tar.gz && cd cognitive-driver-0.1.0-linux-amd64
sudo scripts/install.sh
```

From source:

```bash
git clone https://github.com/spikeloader/cognitive-driver.git && cd cognitive-driver
make build                 # as your user
sudo scripts/install.sh    # or: sudo make install
```

The installer sets up:

| Item | Location |
|---|---|
| Binary | `/usr/local/bin/cognitive-driver` |
| Configuration | `/etc/cognitive-driver/config.yaml`, generated from `configs/config.yaml` with the hostname, `data_dir` at `/var/lib/cognitive-driver` and the API on `127.0.0.1:8088` |
| API token | `/etc/cognitive-driver/env` (random `MICRO_AI_TOKEN`, root 0600) |
| systemd unit | `/etc/systemd/system/cognitive-driver.service`, enabled and started |
| State | `/var/lib/cognitive-driver` (SQLite with events and baselines) |

Reinstalling (upgrading) keeps the configuration, token and state; only the
binary and the unit are replaced and the service is restarted. Options:
`--listen :8088` exposes the UI on the network; `--no-start` only copies the
files. To preview the install without root: `DESTDIR=/tmp/stage scripts/install.sh`.

```bash
sudo cat /etc/cognitive-driver/env             # token for the UI and the API
journalctl -u cognitive-driver -f              # JSON logs
sudo systemctl restart cognitive-driver        # after editing the configuration
cognitive-driver -check -config /etc/cognitive-driver/config.yaml   # validate the configuration
sudo scripts/uninstall.sh [--purge]            # remove (--purge deletes config, token and events)
```

The unit runs as an unprivileged dynamic user with a read-only filesystem,
a syscall filter, no capabilities and limits of 25% CPU and 128 MiB of memory
(`systemd-analyze security` 1.4 — OK). The configuration is validated
(`ExecStartPre=... -check`) before every start.

### End-to-end validation

```bash
scripts/validate-e2e.sh            # against the installed service (asks for sudo to read the token and restart)
scripts/validate-e2e.sh --local    # builds and runs the binary on a separate port, without installing
```

Takes ~6–10 min: waits for the baseline to be learned, generates CPU and disk
load (`stress-ng`, or shell loops and `dd`), then checks the event, the API,
the metrics, the logs, the safe mode and persistence across restarts.

### Development

```bash
make test        # unit tests with -race
make run         # build + run locally with configs/config.yaml (state in ./data)
make dist        # release packages in dist/ (linux amd64/arm64 + SHA256SUMS)
```

## Monitored services (nginx, Apache, Docker, PHP, cron, Redis, Memcached, PostgreSQL, MySQL)

Besides the host (CPU, memory, disk, network via node_exporter), each service
has its own driver that reads the service's exporter. One command enables it:
the script configures the exporter, validates and installs the driver in
`/etc/cognitive-driver/drivers.d/` (without touching the main configuration):

```bash
sudo scripts/enable-service.sh nginx     # stub_status on 127.0.0.1:8090 + prometheus-nginx-exporter
sudo scripts/enable-service.sh apache    # mod_status + prometheus-apache-exporter
sudo scripts/enable-service.sh docker    # metrics-addr in daemon.json (restarts dockerd; asks first)
sudo scripts/enable-service.sh php-fpm   # requires php-fpm_exporter on 127.0.0.1:9253
sudo scripts/enable-service.sh swoole --metrics-url http://127.0.0.1:9501/metrics
sudo scripts/enable-service.sh cron      # driver reading cron-wrap metrics through node_exporter
sudo scripts/enable-service.sh processes # process-exporter: events name the responsible process
sudo scripts/enable-service.sh redis     # prometheus-redis-exporter (REDIS_PASSWORD=... if requirepass)
sudo scripts/enable-service.sh memcached # requires memcached_exporter on 127.0.0.1:9150
sudo scripts/enable-service.sh postgresql # prometheus-postgres-exporter + passwordless "prometheus" role (pg_monitor, peer)
sudo scripts/enable-service.sh mysql     # prometheus-mysqld-exporter + prometheus@localhost (socket auth, read-only grants)
sudo scripts/enable-service.sh nginx --disable   # remove the driver
```

| Driver | Detection | Type |
|---|---|---|
| `ai-nginx-health` | nginx down (`nginx_up ≠ 1`) | rule · critical |
| | dropped connections (accepted − handled > 0): `worker_connections`/file descriptor limit | rule · high |
| | requests/s and active connections off baseline | statistical |
| `ai-apache-health` | Apache down (`apache_up ≠ 1`) | rule · critical |
| | busy workers > 90% of the scoreboard (saturation) | rule · high |
| | requests/s, busy workers and mean request time off baseline | statistical |
| `ai-docker-health` | HEALTHCHECK failures | rule · high |
| | sustained container starts > ~3/min (crash loop) | rule · high |
| | running containers off baseline (drop = containers stopping) | statistical |
| `ai-php-fpm-health` | pool down (`phpfpm_up ≠ 1`) | rule · critical |
| (per pool) | `max_children` reached, listen queue, busy processes > 90% | rule · high |
| | slow requests (slowlog) | rule · medium |
| | requests/s off baseline | statistical |
| `ai-swoole-health` | busy workers > 90% | rule · high |
| (per server) | connections, requests/s, coroutines and task queue off baseline | statistical |
| `ai-cron-health` | last run exited ≠ 0 | rule · high |
| (per job) | job not started for more than 2 intervals (cron stopped, line removed) | rule · high |
| | current run going for more than 1 interval (stuck/overlapping) | rule · high |
| | duration > 80% of the interval | rule · medium |
| `ai-redis-health` | Redis down; connections rejected (`maxclients`); memory > 90% of `maxmemory`; last RDB snapshot failed | rule · critical/high |
| | replica lost its link to the primary | rule · critical |
| | evictions, commands/s, keyspace misses and clients off baseline | statistical |
| `ai-memcached-health` | Memcached down; connections > 90% of the limit (`-c`) | rule · critical/high |
| | evictions, GET misses, commands/s and connections off baseline | statistical |
| `ai-postgresql-health` | PostgreSQL down; connections > 90% of `max_connections` | rule · critical/high |
| (per database) | deadlocks; replica lag > 5 min | rule · high |
| | rollbacks and temp files (per database), transactions/s and connections off baseline | statistical |
| `ai-mysql-health` | MySQL/MariaDB down; replication IO/SQL thread stopped | rule · critical |
| | connections > 90% of `max_connections`; replica lag > 5 min | rule · high |
| | running threads, slow queries, aborted connects, InnoDB row lock waits and queries/s off baseline | statistical |

**Rules** fire without a baseline (after `consecutive_points` scrapes) and
cover unambiguous states; **statistical detection** learns each metric's
pattern. Every event carries a service-specific probable cause and
recommendation.

### Responsible process

When an event is emitted for a metric with `attribute` (`cpu`, `memory` or
`io`), cognitive-driver reads the host's
[process-exporter](https://github.com/ncabatoff/process-exporter) twice, 2 s
apart, and stores in the event the process groups (same command name) that
were consuming the most of that resource. The UI shows the top one as
**Responsible** at the top of the event, with its share of the total and the
next ones; the event list shows its name.

```bash
sudo scripts/enable-service.sh processes                       # local: 127.0.0.1:9256
sudo scripts/enable-service.sh processes --listen 0.0.0.0:9256 # host scraped by a central instance
```

- The process-exporter is looked up at the same address as the host's
  node_exporter, on port `attribution.process_exporter_port` (default 9256;
  0 disables). Nothing else to configure per host.
- CPU, memory and disk metrics of the host have `attribute` by default; any
  driver metric can declare it, and `attribute: none` turns it off.
- It points to who was consuming the resource at that moment, not to a
  confirmed root cause. Network traffic is not attributed (the exporter has
  no per-process network data).
- If the process-exporter is unavailable the event is opened anyway, without
  a responsible process.
- Disk events need `--with-io`. `/proc/<pid>/io` is readable only by the
  process owner, so reading it for other users needs `CAP_DAC_READ_SEARCH`
  (file permission) and `CAP_SYS_PTRACE` (the kernel's ptrace check). The
  option grants both to the process-exporter inside a sandbox (read-only
  filesystem, no `/home`, `/root`, `/etc/shadow`, `/etc/ssh` or sudoers). It is
  opt-in because it widens what the exporter can read. Without it, CPU and
  memory events are attributed and disk events are not.

### PHP

| Scenario | Coverage |
|---|---|
| PHP-FPM | `ai-php-fpm-health`, via [php-fpm_exporter](https://github.com/hipages/php-fpm_exporter) reading each pool's `pm.status_path` |
| mod_php | `ai-apache-health` (Apache workers, latency and saturation) |
| Swoole/OpenSwoole | `ai-swoole-health`: the application exposes `$server->stats()` on `/metrics` with [deploy/swoole/metrics.php](deploy/swoole/metrics.php) (example in the file) |
| PHP CLI | through the cron jobs (below) |

### Cron jobs

Each line in `/etc/cron.d` runs through [cron-wrap](deploy/cron/cron-wrap),
which records per job (start, end, exit code, duration and expected interval)
into node_exporter's textfile collector. The `ai-cron-health` driver reads
these metrics from the `collector.targets` hosts and opens **one event per job**.

```bash
sudo scripts/wrap-cron-d.sh            # dry run: shows the diff, writes nothing
sudo scripts/wrap-cron-d.sh --apply    # installs cron-wrap, backs up to /var/backups and rewrites
sudo scripts/wrap-cron-d.sh --unwrap --apply   # revert
```

- The job always runs and keeps its exit code. If the wrapper cannot write
  its metrics, it only warns on stderr.
- Job name: `<file>.<script>` (e.g. `reports.backup`). The interval (`-i`) is
  derived from regular schedules (`*/5`, `0 3 * * *`, `@hourly`, weekdays).
  Irregular schedules only get the failure alert.
- Commands with `&&`, `|`, `;` and friends run through `$SHELL -c '...'` so the
  wrapper measures the whole line. Lines with `%` and `@reboot` are left as is.
- Re-running is safe: already wrapped lines are kept. Jobs of non-root users
  get an ACL to write into the textfile collector directory
  (`/var/lib/prometheus/node-exporter`, or `CRON_WRAP_DIR`).
- When removing a job on purpose, delete its `cron_<job>.prom`; otherwise the
  overdue alert fires.
- Files owned by packages (dpkg/rpm, such as `anacron`, `e2scrub_all` and
  `php`) are skipped by default, since editing them causes conffile conflicts
  on upgrades. Use `--include-packaged` to wrap them anyway.

### Other services without code

Any Prometheus exporter becomes a driver with a file in `drivers.d/`. The
templates in [deploy/drivers/](deploy/drivers/) show every feature; this is a
trimmed version of the Redis one:

```yaml
drivers:
  - name: ai-redis-health
    targets: [{host: my-host, url: "http://127.0.0.1:9121/metrics"}]
    metrics:
      - name: redis_up
        rule: {equals: 1, severity: critical}
        describe:
          label: Redis availability
          unit: bool
          down:
            cause: Redis stopped or the exporter has no access.
            recommendation: Check systemctl status redis-server and the Redis log.
      - name: redis_evicted_keys_total          # counter → rate per second
        describe: {label: Redis evicted keys, unit: per_second}
      - name: redis_memory_used_bytes
        alias: redis_memory_ratio
        divide_by: {name: redis_memory_max_bytes}
        rule: {above: 0.9}
```

Metric fields: `match`/`exclude` (label regexes), `type`
(counter/gauge/timestamp — timestamp becomes an age in seconds), `alias`,
`group_by` (one feature and one event per label value, e.g. `[job]`, `[pool]`,
instead of summing the series), `subtract`/`divide_by` (derived metrics),
`attribute` (`cpu`/`memory`/`io`/`none`: resource used to name the
responsible process), `describe.en` (the same `label`/`up`/`down` texts in
English, used by the English UI), `rule` (`above`/`below`/`equals` + `severity`), `min_stddev` (absolute floor
for the deviation, useful for small counts) and `describe` (`label`, `unit` —
cores, bytes, bytes_per_second, ratio, per_second, count, milliseconds,
seconds, bool — plus cause/recommendation for `up`/`down`). Validate with
`cognitive-driver -check -config /etc/cognitive-driver/config.yaml`.

## Web UI

Open **http://127.0.0.1:8088/** (redirects to `/ui/`). The UI is embedded in
the binary, loads nothing from external origins and only uses this service's
`/health` and `/events`. It is available in English and Brazilian Portuguese
(selector at the top, remembered per browser; the default follows the
browser language). Events recorded before translations existed get them when
the service starts, as long as their driver is still loaded. It shows:

- service state, scraped hosts and drivers (enabled/disabled);
- counters for open, high/critical, acknowledged, resolved and false-positive events;
- a paginated event list with filters by status, severity, driver and host;
- each event's detail: **recommendation**, what was observed, probable cause,
  deviation intensity (low → critical bands) and evidence per window (1m/5m/15m);
- buttons to **acknowledge**, **resolve**, mark as **false positive** or
  reopen, with a feedback field (`PATCH /events/{id}`).

On the first visit, click **Token** and enter the value of `MICRO_AI_TOKEN`
(kept in `sessionStorage`, or `localStorage` if "Remember" is checked). The
page refreshes every 15 s while visible. Each event has a direct link
(`/ui/#event=<id>`) to paste into tickets or team chat.

## How detection works

| Stage | Rule |
|---|---|
| Feature | Counters (`*_total`) become a per-second rate; label-filtered series are summed (or kept apart per group with `group_by`). CPU = busy cores; disk = fraction of time with active I/O; network = bytes/s; memory = available bytes. |
| Baseline | EWMA of mean and variance per host × metric over 1m, 5m and 15m windows (`alpha = 1 - e^(-interval/window)`). |
| Warm-up | No detection before `warmup_points` observations (default 20 ≈ 5 min). |
| Z-score | `(value - mean) / stddev` against the baseline **before** the point, on the `decision_window` (default 5m). The other windows are shown as evidence. The deviation has a floor of `min_stddev_ratio × |mean|` for near-constant metrics. |
| Anomaly | `|z| ≥ anomaly_threshold` (3.0), in either direction (spike or drop). |
| Severity | low 3–4σ · medium 4–6σ · high 6–10σ · critical ≥ 10σ. Score = `1 - e^(-|z|/2)`. |
| Freeze | Anomalous points stay out of the baseline until `baseline_freeze_points` (10 ≈ 2.5 min) in a row, so a spike does not inflate the deviation and mask its own detection. If the anomaly persists beyond that, the new level is learned. |
| Responsible | For metrics with `attribute`, the process groups consuming the most CPU, memory or disk I/O at emission time (process-exporter, 2 samples). Stored with the peak observation. |
| Event | Emitted after `consecutive_points` (3) anomalous points in a row, with a `cooldown` (5m) per host × metric. There is at most one active (open or acknowledged) event per driver × host × metric: re-emissions become occurrences of it. |
| Action | The recommendation goes through the policy; in `observe_only` it is always blocked (`cognitive_driver_actions_blocked_total`). |

Baselines are saved to SQLite every `baseline_save_interval` and on shutdown,
and restored at startup, so a restart does not require learning again.

## API

`/health` and `/metrics` are public (health check and Prometheus scrape).
The others require `Authorization: Bearer $MICRO_AI_TOKEN` when the variable
is set (without it, the service logs a warning and runs without authentication).

| Method | Endpoint | Purpose |
|---|---|---|
| GET | `/health` | Status, version, drivers (contract version, mode, enabled) and scrape state |
| GET | `/metrics` | Internal metrics in Prometheus format |
| POST | `/predict` | Evaluates a value against the current baseline (without changing it or creating an event) |
| GET | `/events` | Recent events. Filters: `host`, `driver`, `metric`, `severity`, `status`, `since` (RFC3339), `limit` (1–500) |
| GET | `/events/{id}` | Detail with evidence per window, probable cause and recommendation |
| PATCH | `/events/{id}` | SRE feedback: `status` (`open`, `acknowledged`, `resolved`, `false_positive`) and `feedback`. Reopening returns 409 if the same metric already has another active event |

```bash
TOKEN="Authorization: Bearer $MICRO_AI_TOKEN"

curl -s localhost:8088/health | jq

# value in the feature's unit: rate/s for counters (0.41 = disk 41% busy)
curl -s -X POST localhost:8088/predict -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"driver":"ai-disk-health","metric":"node_disk_io_time_seconds_total","value":0.41}' | jq

curl -s "localhost:8088/events?severity=high&limit=20" -H "$TOKEN" | jq
curl -s localhost:8088/events/<id> -H "$TOKEN" | jq

curl -s -X PATCH localhost:8088/events/<id> -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"status":"false_positive","feedback":"scheduled nightly backup"}' | jq

curl -s localhost:8088/metrics | grep ^cognitive_driver_
```

Example `/predict` response with an anomaly (`/predict` texts are in Portuguese;
stored events also carry `translations.en`):

```json
{
  "driver": "ai-disk-health", "host": "local", "metric": "node_disk_io_time_seconds_total",
  "observed": 0.41, "anomaly": true, "score": 0.992, "severity": "high", "zscore": 9.67,
  "learning": false, "streak": 1, "window": "5m",
  "baseline": {"mean": 0.12, "stddev": 0.03},
  "windows": [{"window": "1m", "mean": 0.13, "stddev": 0.03, "zscore": 9.3}, "..."],
  "explanation": "Utilização de I/O de disco em 41,0%, 9,7 desvios-padrão acima da baseline de 5m (média 12,0%, desvio 3,0%).",
  "probable_cause": "Pressão de I/O: carga de leitura/escrita acima do normal ...",
  "recommendation": "Investigar processos com alta leitura/escrita (iotop, pidstat -d) ...",
  "mode": "observe_only"
}
```

If there is no baseline yet for the host/metric, `/predict` returns `409`.
During warm-up it returns `200` with `"learning": true` and `"anomaly": false`.

## Internal metrics

| Metric | Description |
|---|---|
| `cognitive_driver_events_total{driver,metric,severity}` | Events emitted |
| `cognitive_driver_false_positives_total{driver,metric}` | False positives reported via `PATCH /events/{id}` |
| `cognitive_driver_inference_duration_seconds{driver}` | Inference latency |
| `cognitive_driver_predictions_total`, `cognitive_driver_anomalous_points_total` | Prediction volume and points above the threshold |
| `cognitive_driver_scrapes_total{host,result}`, `cognitive_driver_scrape_duration_seconds` | Scrape health |
| `cognitive_driver_actions_blocked_total{driver}` | Actions blocked by the policy |
| `cognitive_driver_driver_info`, `cognitive_driver_store_errors_total` | Loaded drivers and persistence failures |
| `go_memstats_*`, `process_*` | The service's own memory and CPU usage |

Every prediction (`prediction`), event (`anomaly_event`), blocked action
(`action_blocked`) and feedback (`event_feedback`) is logged as JSON on stdout.

## Configuration

See [configs/config.yaml](configs/config.yaml), which documents every field.
Each item in `drivers` is an independent micro-AI with its own metrics and
thresholds, and can be disabled with `enabled: false`. The only mode accepted
in the MVP is `observe_only`; anything else is rejected at validation.

## Layout

```
cmd/cognitive-driver/     main: configuration, wiring, HTTP server, shutdown
internal/config/          YAML + defaults + validation
internal/collector/       HTTP scrape + Prometheus text format parser
internal/feature/         counter → rate, timestamp → age, label filters, aggregation and group_by
internal/attribution/     responsible process from process-exporter
internal/anomaly/         EWMA, z-score, severity, consecutive points, cooldown
internal/explain/         catalog (host, nginx, Apache, Docker), explanation and recommendation
internal/policy/          action authorization (blocks everything in the MVP)
internal/driver/          Driver v1 contract + statistical implementation
internal/runner/          observe → detect → explain → recommend loop
internal/store/           SQLite (events and baselines)
internal/telemetry/       the service's Prometheus metrics
internal/api/             HTTP/JSON endpoints
internal/api/ui/          web UI (HTML/CSS/JS embedded via go:embed)
deploy/systemd/           sandboxed unit with limits
deploy/drivers/           driver templates per service (nginx, apache, docker, php-fpm, swoole, cron,
                          redis, memcached, postgresql, mysql)
deploy/cron/cron-wrap     cron job wrapper (metrics into the textfile collector)
deploy/swoole/metrics.php Swoole/OpenSwoole metrics for the application's /metrics
scripts/                  install.sh, uninstall.sh, enable-service.sh, wrap-cron-d.sh, validate-e2e.sh
```

## Driver contract (v1)

```go
type Driver interface {
    Info() Info                                       // name, version, contract, mode
    Observe(collector.Snapshot) []feature.Feature     // observe(event) -> features
    Predict(feature.Feature) Anomaly                  // predict(features) -> anomaly (learns online)
    Evaluate(feature.Feature) (Anomaly, error)        // side-effect-free predict (/predict)
    Explain(Anomaly) explain.Explanation              // explain(anomaly) -> explanation
    Recommend(Anomaly) policy.Action                  // recommend(anomaly) -> action
    Act(context.Context, policy.Action) error         // act(action): blocked in the MVP
}
```

A new implementation (TinyML, a local model) only needs to satisfy this
interface. The runner, the API and the storage stay the same.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). For vulnerabilities, follow
[SECURITY.md](SECURITY.md) (private report, not an issue). Changes per version
are in [CHANGELOG.md](CHANGELOG.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 The cognitive-driver authors.
