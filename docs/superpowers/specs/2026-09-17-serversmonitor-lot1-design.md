# ServersMonitor — Lot 1: hub, agent and web UI

**Date**: 2026-09-17
**Status**: design approved section by section by Vincent on 2026-09-17; spec awaiting his review
**Scope**: this repository (`ServersMonitor`), lot 1 only. Lots 2–6 are listed in §2 and get
their own spec each.

## 1. What is being built

A self-hosted server monitoring hub in the spirit of [Beszel](https://beszel.dev/): a small agent
on every host pushes system metrics (CPU, memory, disk, network, load, temperatures, Docker
containers) to one hub that stores them, draws them, and raises alerts. On top of that, in later
lots, the same hub reads and manages an Azure sandbox resource group: inventory, costs, start/stop,
VM provisioning, cost guardrails.

Decisions taken during brainstorming, fixed for the whole project:

| Question | Decision |
|---|---|
| Which servers | Any host where the agent is installed (Azure VMs, Raspberry Pi, Macs, hosted servers) **and** the sandbox's current resources (App Services, Container Apps) |
| Relation to `AzureSandboxManager` / `SandboxWatch` | **None.** Independent product, own Azure reads, own source of truth |
| Hosting | Portable: one Docker image, config by environment variables, hosting decided later |
| "Manage the sandbox" means | Read-only inventory/state/costs; start/stop/restart; create/delete VMs; cost guardrails |
| Stack | Go for hub and agent (static binaries), SvelteKit static export embedded in the hub |
| Alert channels (lot 2) | SMTP, generic webhook / ntfy, Microsoft Teams |
| Authentication | One local admin account with password; Entra ID may be added later |
| Names | Project `ServersMonitor`, binaries `smhub` and `smagent`, image `serversmonitor/hub` |

## 2. Lots

| Lot | Content | Depends on |
|---|---|---|
| **1. Core** (this spec) | Hub + agent, metrics, SQLite with retention, web UI with charts, local admin, alert rules shown in the UI | — |
| 2. Alert channels | SMTP, webhook/ntfy, Teams; one delivery per transition (fired / resolved) | 1 |
| 3. Azure read | Resource group inventory, state of web apps / Container Apps / VMs, HTTP probes, cost vs budget | 1 |
| 4. Azure actions | Start/stop/restart VMs, web apps, Container Apps; confirmation, audit log, subscription guard | 3 |
| 5. VM provisioning | Create a Linux VM (size, image, region) with the agent installed by cloud-init; clean deletion (disk, NIC, IP) | 4 |
| 6. Cost guardrails | Stop schedules (nights, weekends), orphan detection, budget alerts | 4 |

Architecture choice for all lots: **monolithic Go hub, outbound Go agent**. The agent connects
*to* the hub over WebSocket with a per-host token. This direction is the one that survives every
hosting choice: a Raspberry Pi behind a home router or a VM without a public IP reaches the hub
without opening a port. Two alternatives were rejected: the hub polling agents over SSH (classic
Beszel) requires the hub to reach every host; reusing Beszel itself with an Azure sidecar keeps
two products and a PocketBase coupling that cannot be extended without a fork.

Lot 1 explicitly does **not** do: any Azure call, any outbound notification, more than one user,
native TLS (a reverse proxy terminates it, or plain HTTP on localhost).

## 3. Components and flow

One Go module, two binaries.

```
serversmonitor/
├── cmd/smhub/            # hub entry point
├── cmd/smagent/          # agent entry point
├── internal/hub/
│   ├── server/           # HTTP: API routes, sessions, embedded static files, SSE
│   ├── ingest/           # agent WebSocket endpoint, validation, writes
│   ├── store/            # SQLite: schema, migrations, queries, retention
│   ├── alerts/           # rule evaluation, state machine, event log
│   └── auth/             # local admin, argon2id, sessions, rate limiting
├── internal/agent/
│   ├── collect/          # cpu, mem, disk, net, load, temp, docker
│   └── link/             # outbound connection, reconnection, buffering
├── internal/proto/       # message types shared by hub and agent (JSON)
├── web/                  # SvelteKit, static export → go:embed
└── deploy/               # Dockerfile, compose, agent install.sh (systemd / launchd)
```

**Nominal flow.** The agent starts with two values: the hub URL and its token. It opens an
outbound WebSocket, authenticates, then sends one full sample every 10 s (configurable by the hub).
The hub validates the message, writes it to SQLite, updates the host's online status, and pushes
the update to connected browsers over Server-Sent Events. Every minute a ticker evaluates alert
rules on the latest values; every hour another one aggregates and purges according to retention.

**Offline detection is the hub's decision, not the agent's.** A host with no message for
3 send intervals becomes `offline`; that is an alert transition like any other. Silence is
information: it is never rendered as a zero value.

**Docker on the agent.** Read-only access through `/var/run/docker.sock` mounted into the
container, or a socket proxy. If the socket is absent, the Docker section is marked *unavailable*,
not empty.

## 4. Data model and retention

SQLite via `modernc.org/sqlite` (pure Go, no CGO), WAL mode, one file under the data directory.

| Table | Role | Key |
|---|---|---|
| `hosts` | A registered host: name, token hash, status (`online`, `offline`, `never_seen`), `last_seen`, system info (OS, arch, cores, total RAM, agent version), muted flag | `id` |
| `samples` | Raw samples as received, every 10 s | `(host_id, at)` |
| `samples_10m`, `samples_1h`, `samples_1d` | Aggregates: avg, min, max per metric | `(host_id, at)` |
| `containers` | Last known state of each Docker container per host (name, image, status, cpu %, memory, network) | `(host_id, name)` |
| `container_samples` | Container history, same retention scheme as `samples` | `(host_id, name, at)` |
| `alert_rules` | Rule: scope (all hosts or one), metric, threshold, duration before firing | `id` |
| `alert_events` | Append-only log of transitions: `fired` / `resolved`, observed value, timestamp | `id` |
| `users`, `sessions` | Local admin and cookie sessions | `id` |
| `settings` | Key/value: agent interval, retentions, schema version | `key` |

**Sample contents.** CPU %, memory (used, total, swap used, swap total), disks per mount point
(used, total, read B/s, write B/s), network (sent B/s, received B/s, cumulative counters),
load 1/5/15, temperatures per sensor, uptime. Scalars are fixed columns; lists (disks, sensors)
are JSON columns, which avoids one table per list while staying queryable with SQLite's JSON
functions. A metric the agent did not collect is stored as `NULL`, never as `0`.

**Retention** (defaults, editable in `settings`):

| Table | Kept |
|---|---|
| `samples` | 24 h |
| `samples_10m` | 30 days |
| `samples_1h` | 1 year |
| `samples_1d` | forever |

Aggregation runs hourly and is **idempotent**: it recomputes the last complete window and never
touches the window in progress, so a restarted hub produces neither gaps nor duplicates. Charts
pick the table from the requested period: 1 h and 24 h → raw, 7 days → 10 min, 30 days → 1 h,
longer → 1 day.

**Migrations.** Numbered SQL files embedded in the binary, applied at startup inside one
transaction, current version stored in `settings`.

## 5. Agent ↔ hub protocol

**Enrolment.** In the UI, *Add host* creates the row and shows once a token (32 random bytes,
base64url) and the ready-to-paste install command:

```
curl -fsSL https://hub.example/install.sh | sudo sh -s -- --hub wss://hub.example --token <token>
```

The hub stores only the token hash. Regenerating a token invalidates the previous one at the next
reconnection.

**Connection.** WebSocket on `/agent/ws`, token in `Authorization: Bearer`. One active socket
per host: a second connection with the same token replaces the first, which covers a restarted
agent whose previous socket has not yet timed out on the hub side.

**Messages** (JSON, one type per message, field `v` for the protocol version):

| Direction | Type | Content |
|---|---|---|
| agent → hub | `hello` | agent version, OS, arch, hostname, cores, total RAM, capabilities (`docker`, `temps`) |
| hub → agent | `welcome` | send interval, mount points to ignore |
| agent → hub | `sample` | one full sample (§4) |
| hub → agent | `reconfigure` | new interval, without reconnecting |
| both | ping / pong | native WebSocket control frames, every 30 s |

**Compatibility rule.** A newer agent sends unknown fields: the hub ignores them. A newer hub reads
fields an older agent does not send: it treats them as *not collected*, never as zero. A test
fails if this rule is removed.

**Reconnection.** Exponential backoff on the agent (1 s → 60 s, with jitter). During an outage the
agent keeps the last 60 samples in memory and replays them in a burst on reconnection, which limits
chart gaps. The hub accepts past-dated samples as long as they are newer than the last one received
for that host.

**Security.** A token only authorizes sending samples for its host; no inbound message can read
or modify anything else. Message size capped at 256 KB, rate capped at one sample per second,
disconnection on a malformed message. TLS is the reverse proxy's job, but `smagent` refuses
`ws://` outside localhost unless `--insecure` is given explicitly.

## 6. Web UI

Beszel is the visual reference: a host grid, one page per host with charts, light/dark theme.
Four screens in lot 1.

**Login.** On first start, if no user exists, the page offers to create the admin. Afterwards:
e-mail, password, `HttpOnly` + `SameSite=Strict` session cookie, five failures = one minute
lockout.

**Dashboard.** A table (cards on narrow screens), one row per host: name, status dot, CPU %,
memory %, root disk %, network ↑↓, uptime, agent version. Percentage cells are thin gauges; the row
is tinted when an alert is firing. Live updates over SSE, no reload. Sort by column, filter by
name.

**Host detail.** Period selector (1 h, 24 h, 7 d, 30 d, 1 y). Stacked charts: CPU, memory + swap,
disks (one per mount point), disk I/O, network, load, temperatures. Then the Docker container table
with per-container CPU/memory/network and a sparkline. Then this host's recent alerts. Charts use
`uPlot`; data comes from `GET /api/v1/hosts/{id}/series?period=`.

**Settings.** Three tabs: *Hosts* (add, rename, regenerate token, delete with confirmation, show
the install command), *Alerts* (global default rules and per-host exceptions), *System* (interval,
retention, password change).

**API.** Everything the UI shows goes through `/api/v1` as JSON, protected by the session. This is
the surface a future client (SandboxWatch or another) can consume without changing the hub.

**Front stack.** SvelteKit with `adapter-static`, TypeScript, Tailwind for theming, no heavy
component library. The build produces `web/build/`, embedded with `go:embed`; `go build` alone
suffices as long as the folder exists, `make web` regenerates it. Node is a build-time dependency
only, never a runtime one.

## 7. Alerts

**A rule = (scope, metric, threshold, duration).** Scope: all hosts or one host; the per-host
exception wins over the global rule. Lot 1 metrics: `status` (offline), `cpu`, `memory`, `disk`
(per mount point, the worst one counts), `load` (5 min load divided by core count), `temperature`
(hottest sensor), `bandwidth` (network throughput, sent + received, threshold in MB/s). The duration prevents flapping: the threshold
must be exceeded for the whole duration (e.g. CPU > 90 % for 10 min) before firing.

**State machine per (rule, host).** `ok` → `pending` on the first breach → `firing` once the
duration is reached → `ok` as soon as the value drops below the threshold. Only the transitions
`pending → firing` and `firing → ok` write a row in `alert_events`; `pending` is internal and never
logged. Lot 2 will attach delivery channels to exactly these two transitions and nowhere else.

**Offline.** An implicit `status` rule exists for every host, duration = 3 send intervals. It
cannot be deleted, only muted per host (e.g. a laptop that gets switched off). A `never_seen` host
triggers nothing: it was never seen, so it cannot have disappeared.

**Silence ≠ zero.** The evaluator only reads samples newer than 2 intervals. A host without a
fresh sample is evaluated by the `status` rule only, never by thresholds; otherwise a stopped agent
would resolve every CPU alert to `ok` while nothing is known. Dedicated test.

**Defaults created at install**: CPU > 90 % (10 min), memory > 90 % (10 min), disk > 90 %
(30 min), temperature > 80 °C (5 min). Editable, deletable.

**Display.** Banner above the dashboard with the count of firing alerts, an *Alerts* page listing
firing ones then paginated history, tinted host rows.

## 8. Errors, tests, packaging

**Errors.** The hub never stops because of an agent: invalid message → that agent is disconnected,
logged, counted. SQLite locked → short retry, then the sample is dropped and counted, never
blocking the WebSocket. Corrupt database at startup → exit with a clear message, no silent
recreation. On the agent, a failing collector (no sensor, no Docker) marks its section
*unavailable* and the others carry on.

**Tests.** TDD; `go test ./...` runs without network or Docker.
- `store`: migrations on an empty database; retention and idempotent aggregation (running twice
  gives the same result); table choice by period.
- `alerts`: the state machine, the duration, silence ≠ zero, `never_seen` is inert.
- `ingest`: hello/welcome, invalid token, connection replacement, oversized message, sample older
  than the last received, unknown field ignored, missing field ≠ zero.
- `agent/collect`: every collector behind an interface, tested with fixed `/proc` fixtures and a
  fake Docker client.
- `auth`: first admin creation, hashing, rate limiting, session expiry.
- One end-to-end test: in-memory hub + real agent on an ephemeral port, one sample crosses
  everything and comes out of `/api/v1`.

Front: `vitest` on utilities (formatting, period choice); no rendering tests in lot 1.

**Packaging.**
- `Makefile`: `web`, `build`, `test`, `docker`, `release` (cross-compilation linux/amd64,
  linux/arm64, darwin/arm64 for both binaries; artefacts under `release/`).
- Hub: multi-stage `Dockerfile` (Node for the front, Go for the binary, final image `scratch` +
  CA certificates), volume `/data` for SQLite, example `docker-compose.yml` with an optional Caddy
  reverse proxy.
- Agent: `deploy/install.sh` (downloads the binary, systemd unit on Linux, `launchd` on macOS) and a
  Docker image for those who prefer it (`--pid=host`, `/proc` and the Docker socket mounted
  read-only).
- Configuration by environment variables (`SM_LISTEN`, `SM_DATA_DIR`, `SM_AGENT_INTERVAL`, …) with
  equivalent flags.
- CI GitHub Actions: `go vet`, `go test`, front build, image build.

**Repository.** New git repository in this folder; feature branch then PR, never a direct push to
`main`. `.gitignore` covers the five journal files, `release/`, `web/build/`, `*.db`.

## 9. Open points carried to later lots

- **Azure identity.** Creating a service principal in the target tenant may be forbidden. The
  hub will use the Azure SDK default credential chain (environment → managed identity → `az` CLI),
  so lot 3 is not blocked; the actual mechanism is verified when lot 3 starts.
- **TLS.** Native TLS / ACME in the hub is deferred; a reverse proxy is assumed.
- **Multi-user and Entra ID.** Deferred; the session layer is designed so a second identity
  provider can be added without touching `/api/v1`.
