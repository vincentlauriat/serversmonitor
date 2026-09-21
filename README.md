# ServersMonitor

[![ci](https://github.com/vincentlauriat/serversmonitor/actions/workflows/ci.yml/badge.svg)](https://github.com/vincentlauriat/serversmonitor/actions/workflows/ci.yml)

**A self-hosted monitoring hub — one small agent per machine, one hub that stores, draws and
alerts.**

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

## Azure

ServersMonitor reads an Azure resource group: what is in it, what state it is in, and what it has
cost so far this month, can start, stop or restart an App Service or a VM in it, and can create a
Linux VM with the agent already installed.

| It needs | Detail |
|---|---|
| A credential | Either an app registration with a client secret, or a managed identity on the machine running the hub. |
| The **Reader** role | On each resource group, granted by a tenant administrator. Enough to read everything, cost included. |
| The **Website Contributor** role | Only to *act* on App Services. Reader can see one; it cannot start or stop it. |
| The **Virtual Machine Contributor** role | Only to create, act on and delete VMs. Ask for all three roles at once — a second round trip through a tenant administrator is a second wait. |
| A VNet with a subnet | **You create it, not the hub.** Virtual Machine Contributor cannot create a VNet, a public IP or an NSG; it can only join an existing subnet. Being Contributor on the resource group is enough to make one yourself, with no administrator involved. |
| Outbound internet on that subnet | Azure retired implicit outbound access for new deployments on 2025‑09‑30, so a subnet created since then returns `defaultOutboundAccess: false` and a VM with no public IP cannot reach anything — including this hub. Set it back to `true` (still accepted, deprecated) or put a NAT Gateway on the subnet (~€32/month). |
| At least one resource group | Reading a whole subscription would need a subscription-scope role assignment this hub does not ask for. |

**Reader covers cost as well as inventory.** The Cost Management query is an HTTP POST, which looks
like a write and is not: its operation is `Microsoft.CostManagement/query/read`. No Contributor role
and no separate billing role is needed.

Configure it under **Settings → Azure**, then press **Test connection**, which reports Azure's own
error rather than a generic failure — usually a role assignment that was never made.

**A failed sync keeps the last good inventory** and says so in red above the table. It never empties
it: an agent that cannot reach Azure must not look like a sandbox with nothing in it. In the same
spirit, a resource Azure has not billed shows a dash, never `0.00`.

### Actions

Start, stop and restart, on App Services and VMs. On a VM, **stop means deallocate**: powering it
off while keeping it allocated would keep the bill running, which is the opposite of why anyone
presses Stop. Two more rules are worth knowing because they are deliberate:

- **The state shown afterwards is read back from Azure**, never inferred from the action you asked
  for. If Azure has not caught up, the table says `Running` after a stop, because that is what Azure
  said. If the read fails, the previous state stands and nothing is invented.
- **An action interrupted by the hub stopping is never replayed.** It is recorded as *interrupted*
  and left there. Re-firing a stop at startup could stop a resource you restarted by hand in the
  meantime — the opposite choice from a pending notification, which *is* replayed, because a lost
  alert is worse than a duplicate one.

Every action is logged with its outcome and Azure's own error, under the table.

### Creating a virtual machine

From the Azure page: a name, and the machine comes up with the agent installed and reporting.

- **It gets no public IP, and does not need one.** The agent dials out, so a machine with no inbound
  address is exactly as monitorable — and nobody can SSH to it from outside. The other side of that
  bargain: **the hub needs an address the VM can reach.** `localhost` is refused at the form rather
  than discovered twenty minutes later as a machine that booted and never called home.
- **The hub never creates a network.** You paste the id of a subnet you made; the VM joins it.
- **Nothing is ever rolled back.** A creation that fails halfway leaves a resource behind, and the
  page names it, with the button that deletes it. The hub does not delete anything on its own —
  including after its own failure, which is the least exercised path it has.
- **Deleting is confirmed by typing the VM's name**, and only ever touches resources tagged
  `createdBy=ServersMonitor`. A resource you created by hand cannot be reached by a mistyped id.

⚠️ **No real subscription has ever been read, acted on or provisioned by this code.** Every Azure
endpoint in the test suite is a local fake. The error path *has* been checked against real Azure — a
wrong tenant id returns `AADSTS900021`, and that message reaches the action log and the browser
intact, trace id included — but the first successful sync, the first resource that actually stops,
and the first VM that actually boots will be yours. That last one is the whole point of lot 5, and
it stays unproven.

## What this does not do yet

- **No published agent binary yet.** cloud-init installs the agent from the GitHub release, and
  there is no release to download. Until one is published, a created VM comes up without an agent.
- **The hub does not check the subnet can reach the internet.** It cannot: reading it is one
  permission and fixing it is another, and both are outside the role it is given. If
  `defaultOutboundAccess` is false and there is no NAT Gateway, the VM boots and the agent never
  connects, with nothing on the hub's side to say why.
- **No cost guardrails.** Schedules, orphan detection and budget alerts arrive in lot 6.
- **No per-rule routing.** Every enabled channel receives every transition. Mute a host to silence it.
- **One user.** A single local admin account; Entra ID is deferred.
- **No TLS of its own.** Put a reverse proxy in front for anything but localhost.

## Roadmap

| Lot | Content | State |
|---|---|---|
| 1 | Hub, agent, web UI, alerts | **done** |
| 2 | Alert channels: SMTP, webhook / ntfy, Teams | **done** |
| 3 | Azure read: inventory, state, costs vs budget | **done** |
| 4 | Azure actions: start, stop, restart | **done** |
| 5 | VM provisioning with the agent pre-installed | **done** |
| 6 | Cost guardrails: schedules, orphans, budget alerts | |

## Design notes

[docs/ARCHITECTURE_EN.md](docs/ARCHITECTURE_EN.md) is the source of truth for technical decisions,
with a French mirror in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The design specs and the
implementation plans live in `docs/superpowers/`. They record the decisions and,
more usefully, the things that only showed up against real data — a Mac reporting eight mount points
of which seven are noise, thirty-nine temperature sensors, a hub exiting 0 on a port that was already
taken, a duration formatter that turned `10m` into `1`, a test notification that linked to a host
that does not exist, and Azure settings that synced nothing for an hour after being saved.
