# ServersMonitor

[![ci](https://github.com/vincentlauriat/serversmonitor/actions/workflows/ci.yml/badge.svg)](https://github.com/vincentlauriat/serversmonitor/actions/workflows/ci.yml)

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

There is no Docker Hub image or GitHub release yet, so build it from this repository:

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

## Notifications

Every `fired` and `resolved` transition is sent to whichever channels are enabled under
**Settings → Notifications**. Nothing else is: a threshold crossing that does not change state
produces no message, and a muted host produces none at all.

| Channel | What it needs |
|---|---|
| Email | An SMTP server, a port, a sender and at least one recipient. STARTTLS, implicit TLS or plain. |
| Webhook | Any URL. A JSON body carries the host, metric, value, threshold and a link; ntfy also reads the title and priority from headers, which are sent too. Extra headers are configurable, for a bearer token. |
| Microsoft Teams | The HTTP URL of a Power Automate workflow using the *When a Teams webhook request is received* trigger. |

Set **Public URL of this hub** so each message links back to the host page. Left empty, messages
carry no link at all: a link pointing at the wrong place is worse than none.

**Delivery is at least once.** A failed send is retried after 1 s, 5 s and 25 s, and the outcome of
every attempt is recorded in the delivery log below the settings. Because the intent to deliver is
written before the first attempt, a hub that crashes between a successful send and recording it will
repeat that message when it restarts. A duplicate e-mail is a nuisance; a lost alert is the failure
this exists to prevent.

Each channel has a **Send test** button that reports the channel's own error, not a generic failure.

⚠️ **Teams is not verified against a live tenant.** The payload is asserted byte for byte against the
Adaptive Card contract Microsoft documents, and the HTTP behaviour is tested, but no message has been
delivered to a real Teams channel from this code. Office 365 connector URLs
(`outlook.office.com/webhook/…`) stopped working in May 2026 and are not supported.

## What lot 2 does not do

- **No Azure.** Reading and managing an Azure sandbox resource group is lots 3 to 6.
- **No per-rule routing.** Every enabled channel receives every transition. Mute a host to silence it.
- **One user.** A single local admin account; Entra ID is deferred.
- **No TLS of its own.** Put a reverse proxy in front for anything but localhost.

## Roadmap

| Lot | Content | State |
|---|---|---|
| 1 | Hub, agent, web UI, alerts | **done** |
| 2 | Alert channels: SMTP, webhook / ntfy, Teams | **done** |
| 3 | Azure read: inventory, state, costs vs budget | next |
| 4 | Azure actions: start, stop, restart | |
| 5 | VM provisioning with the agent pre-installed | |
| 6 | Cost guardrails: schedules, orphans, budget alerts | |

## Design notes

[docs/ARCHITECTURE_EN.md](docs/ARCHITECTURE_EN.md) is the source of truth for technical decisions,
with a French mirror in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The design specs and the
implementation plans live in `docs/superpowers/`. They record the decisions and,
more usefully, the things that only showed up against real data — a Mac reporting eight mount points
of which seven are noise, thirty-nine temperature sensors, a hub exiting 0 on a port that was already
taken, a duration formatter that turned `10m` into `1`, and a test notification that linked to a host
that does not exist.
