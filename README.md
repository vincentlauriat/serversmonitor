# ServersMonitor

**A self-hosted monitoring hub in the spirit of [Beszel](https://beszel.dev/) — one small agent per
machine, one hub that stores, draws and alerts.**

Two Go binaries, no runtime dependency. `smhub` carries the web interface inside itself; `smagent`
runs anywhere you can put a static binary: an Azure VM, a Raspberry Pi, a Mac, a box at a hosting
provider.

```
┌──────────┐   outbound WebSocket   ┌────────────────────────┐
│ smagent  │ ─────────────────────► │ smhub                  │
│ every    │   token per host       │  SQLite + retention    │
│ 10 s     │                        │  alert state machine   │
└──────────┘                        │  /api/v1 + SSE         │
                                    │  embedded web UI       │
                                    └────────────────────────┘
```

The agent connects **to** the hub, never the other way round. That is what lets a Pi behind a home
router or a VM without a public address be watched without opening a single port.

## What it reports

| | |
|---|---|
| Machine | CPU, memory and swap, disks per mount point, disk I/O, network, load, temperatures, uptime |
| Docker | Every container: state, CPU, memory, network — read-only, through the socket |
| Alerts | CPU, memory, disk, load, temperature, bandwidth, plus a built-in offline rule |

## The rule that makes it trustworthy

**A metric that was not collected is never shown as zero.** It is `nil` in the protocol, `NULL` in
SQLite, `null` in the API, a dash in the table and a gap in the chart. A host that has gone quiet is
judged by the offline rule alone — never by thresholds it can no longer report on. There are tests
that fail if this stops being true.

Without it, an agent that dies looks like a machine whose CPU just dropped to 0 %, and every alert
it had resolves itself at the worst possible moment.

## Quick start

Nothing is published yet — no Docker Hub image, no GitHub release. Build it:

```
docker build -f deploy/Dockerfile -t serversmonitor/hub .
docker run -d --name smhub -p 8090:8090 -v smhub-data:/data serversmonitor/hub
```

Open <http://localhost:8090>, create the admin account, add a host, and run the command it gives you
on the machine you want to watch. See [docs/getting-started.md](docs/getting-started.md), which also
covers installing the agent while there is no release to download it from.

## Build from source

Go 1.26 and Node 24 (Node only to build the front).

```
make web build    # bin/smhub and bin/smagent, front embedded
make test         # go test ./...
make release      # linux/amd64, linux/arm64, darwin/arm64 in release/
```

## What lot 1 does not do

- **No notifications.** Alerts show in the interface; e-mail, ntfy and Teams come in lot 2.
- **No Azure.** Reading and managing an Azure sandbox resource group is lots 3 to 6.
- **One user.** A single local admin account; Entra ID is deferred.
- **No TLS of its own.** Put a reverse proxy in front for anything but localhost.

## Roadmap

| Lot | Content | State |
|---|---|---|
| 1 | Hub, agent, web UI, alerts | **done** |
| 2 | Alert channels: SMTP, webhook / ntfy, Teams | next |
| 3 | Azure read: inventory, state, costs vs budget | |
| 4 | Azure actions: start, stop, restart | |
| 5 | VM provisioning with the agent pre-installed | |
| 6 | Cost guardrails: schedules, orphans, budget alerts | |

## Design notes

The design and the implementation plan live in `docs/superpowers/`. They record the decisions and,
more usefully, the things that only showed up against real data — a Mac reporting eight mount points
of which seven are noise, thirty-nine temperature sensors, a hub exiting 0 on a port that was already
taken.
