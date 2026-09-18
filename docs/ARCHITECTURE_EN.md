# ServersMonitor — Architecture

Source of truth for technical decisions. `ARCHITECTURE.md` is its French mirror and must be edited
in the same commit.

State: lots 1 and 2 delivered. Lots 3 to 6 (Azure) are roadmap, and nothing below describes them.

## The one invariant

**Silence is never zero.**

A metric that was not collected is `nil` in the protocol, `NULL` in SQLite, `null` in the API, a dash
in the table and a gap in the chart. It is never `0`. A host that stops reporting is judged by the
offline rule alone, never by thresholds it can no longer report on.

This is not a style preference. An agent that dies looks exactly like a machine whose CPU dropped to
0 %, and every alert it had would resolve itself at the worst possible moment. Every layer below
carries this rule, which is why optional sample fields are pointers, aggregation carries nulls
through, and the evaluator skips a host whose newest sample is older than two intervals.

## Shape

```
┌──────────────┐   outbound WebSocket, bearer token per host   ┌───────────────────────────┐
│   smagent    │ ───────────────────────────────────────────►  │           smhub           │
│  (one per    │        proto.Sample every N seconds           │                           │
│   machine)   │ ◄───────────────────────────────────────────  │  ingest → store           │
└──────────────┘        proto.Welcome / Reconfigure            │      ↓                    │
                                                               │   alerts (state machine)  │
                                                               │      ↓                    │
                                                               │   notify → deliveries     │
                                                               │      ↓                    │
                                    browser ◄── SSE + REST ──  │   server (+ embedded web) │
                                                               └───────────┬───────────────┘
                                                                           │
                                                          SMTP · webhook/ntfy · Teams
```

One process for the hub, one per watched machine, one SQLite file. No message broker, no separate
time-series database, no reverse proxy of its own.

### Why the agent dials out

The agent opens the connection, not the hub. A Raspberry Pi behind a home router, a laptop on a
café network and an Azure VM without a public address all work with no port forwarding and no
inbound firewall rule. The cost is that the hub cannot reach an agent that has not called in, which
is why the hub, not the agent, decides a host is offline.

## Packages

| Package | Responsibility |
|---|---|
| `internal/proto` | The JSON messages on the wire. No logic beyond encode and decode. |
| `internal/agent/collect` | Reads the machine: cpu, memory, disk, network, load, temperatures, Docker. |
| `internal/agent/link` | The outbound WebSocket, reconnection, and the buffer that covers a short outage. |
| `internal/hub/store` | The SQLite database: schema, migrations and every query. No other package writes SQL. |
| `internal/hub/ingest` | Authenticates an agent, reads its samples, writes them through the store. |
| `internal/hub/alerts` | The state machine and the evaluator. Pure: no database, no clock of its own. |
| `internal/hub/notify` | Message rendering, the three channels, and the dispatcher. |
| `internal/hub/azure` | Entra tokens, the ARM client, the inventory sweep and the cost query. No Azure SDK. |
| `internal/hub/azure` | Entra tokens, the ARM client, the inventory sweep and the cost query. No Azure SDK. |
| `internal/hub/auth` | Argon2id password hashing and the login rate limiter. |
| `internal/hub/server` | The REST API, Server-Sent Events, and the embedded front-end. |
| `internal/hub/config` | Environment variables. Read once at startup. |
| `internal/hub` | Wiring and the periodic jobs. The only package that knows about all the others. |
| `web/` | SvelteKit 5, static export, embedded into the binary at build time. |

### Dependency direction

`hub` depends on everything; nothing depends on `hub`. `server` does not import `hub`: it declares a
`Notifier` interface that `hub` satisfies, which is what keeps the cycle from forming. `alerts` and
`notify` import `store` for its types but never reach for a database of their own, so both are
testable without one.

## The protocol

Four message types, one version constant, `internal/proto`.

| Message | Direction | Purpose |
|---|---|---|
| `hello` | agent → hub | First message: agent version, OS, arch, hostname, cores, total memory. |
| `welcome` | hub → agent | Sampling interval and the mount points to ignore. |
| `sample` | agent → hub | One snapshot, every interval. |
| `reconfigure` | hub → agent | A new interval, without reconnecting. |

**Every optional field in `Sample` is a pointer.** `CPU *float64`, not `float64`. A nil pointer means
the agent did not collect it. Decoding ignores unknown fields and leaves missing ones nil, so an
older agent talking to a newer hub degrades rather than fails.

Authentication is a bearer token per host, generated by the hub and shown once. The store keeps only
its SHA-256. Regenerating a token invalidates the old one immediately.

## The agent

`collect` reads through an interface, so the tests never touch the real machine. `gopsutil.go` is the
only file that calls `gopsutil`; `docker.go` speaks HTTP directly over the Docker unix socket, two
endpoints, rather than pulling in the Docker SDK.

Two things learned from real data rather than from tests:

- **Mount noise.** This Mac reports eight mount points, seven of which are duplicates of the same
  APFS container or system volumes nobody watches. A prefix list plus deduplication by
  `(used, total)` brings it to one. The list is sent by the hub in `welcome`, so it can change
  without redeploying agents.
- **Sensor count.** The same Mac reports thirty-nine temperature sensors. `sensors()` ranks them by
  peak and the host page charts the top six, saying so.

`link` reconnects with backoff and keeps the last few samples that could not be sent, oldest first,
so a short outage leaves a gap no longer than the outage itself.

## The store

SQLite through `modernc.org/sqlite`, pure Go, so the binary builds with `CGO_ENABLED=0` and the
Docker image ends at `scratch`.

Migrations are numbered SQL files embedded with `go:embed`, applied in order inside a transaction,
with the version in a `schema_version` table. Timestamps are stored as RFC 3339 UTC strings, so
lexical order is chronological order.

### Tables

| Table | Holds |
|---|---|
| `hosts` | One row per machine: name, token hash, status, last seen, hello fields, muted flag. |
| `samples` | Raw samples. |
| `samples_10m`, `samples_1h`, `samples_1d` | Rolled-up averages. |
| `containers`, `container_samples` | Docker containers and their series. |
| `alert_rules` | Metric, threshold, duration, optional host. |
| `alert_events` | Append-only log of `fired` and `resolved` transitions. |
| `deliveries` | One row per (event, channel): `pending`, `sent` or `failed`. |
| `users`, `sessions` | The single admin account and its cookies. |
| `settings` | Key/value: interval, retentions, and all notification and Azure configuration. |
| `azure_resources` | One row per resource, as the last successful sweep saw it. `state` and `deleted_at` are nullable. |
| `azure_costs` | One row per (resource, month). **No foreign key, deliberately.** |
| `azure_sync` | One row per scope: whether the last inventory and the last cost query worked, and why not. |
| `azure_actions` | One row per start, stop or restart, written before the call. A partial unique index allows one in flight per resource. |

Every series table cascades from `hosts`, and `deliveries` cascades from `alert_events`. Deleting a
host removes everything about it in one statement.

**The DDL of `samples_1h` and `samples_1d` is written out in full rather than derived with
`CREATE TABLE ... AS SELECT`.** That form drops every constraint, including the foreign key, so the
cascade would have silently stopped working and a deleted host would have left rows behind.

### Aggregation and retention

An hourly job rolls raw samples into 10-minute, hourly and daily windows. Only complete windows are
written, and rewriting a window produces the same result, so the job is safe to run at any time,
including twice, including after a crash. Retention is configurable per resolution; daily averages
are kept forever. Settled deliveries are purged after 30 days; a pending one never is, because it is
work not yet done.

## Alerts

`alerts` is pure. `Evaluate` takes the hosts, the newest sample of each, the rules and the current
time, and returns the transitions to persist. It owns no clock and no database, so its tests state a
situation and assert an outcome.

Rules are per metric, with an optional host. `applicable` keeps exactly one rule per metric for a
host, preferring the host-specific one over the global one. A second global rule for the same metric
is ignored.

The **offline rule is implicit** and has no row: a host is offline after three missed intervals, and
that decision is the hub's. It carries rule id `0`.

Only `pending → firing` and `firing → ok` are recorded. A metric that stays above its threshold
produces one event, not one per evaluation.

A muted host is skipped entirely. A rule deleted while firing leaves a `fired` row that nothing will
ever resolve, which is correct for an append-only log and wrong for a badge, so the firing count
filters both cases at display time rather than writing a synthetic `resolved`. The log stays
truthful.

## Notifications

`notify` renders a `Message` once and hands it to each enabled `Channel`, a one-method interface.

**A delivery row is written before the first attempt.** That is what lets a hub killed mid-retry
replay the work on the next boot instead of losing it silently. The consequence is accepted and
stated in the interface: **delivery is at least once**. A crash between a channel's 200 and the
record repeats the message. A duplicate e-mail is a nuisance; a lost alert is the failure this
exists to prevent.

The dispatcher is one worker over a queue held as a slice behind a mutex, not a buffered channel,
because the drop policy has to inspect what is already queued.

- **Retries** at 1 s, 5 s, 25 s, four attempts. `429` and `5xx` come back; other `4xx` do not,
  because they will fail identically forever. The recorded attempt count is the one that actually
  ran, not one derived from the last error's class.
- **`Enqueue` never blocks.** It runs on the hub's own goroutine, and a dead webhook must not stop
  the hub from evaluating alerts.
- **A full queue drops the oldest `fired` first**, a `resolved` only when nothing else is left. Drop
  the resolve and the last thing anyone heard about a host is that it broke. Every drop is recorded
  as a failed delivery with the reason `queue full`.

Channel configuration lives in `settings`, not in the environment, so it is editable from the browser
without restarting. The SMTP password never leaves the API: reads expose `smtp_password_set` only;
on write, an absent field means leave it alone and an empty string means clear it.

**Teams has never delivered to a real tenant from this code.** The Adaptive Card payload is asserted
byte for byte against the contract Microsoft documents and the HTTP behaviour is tested, but a
workflow URL belongs to a tenant. Office 365 connector URLs stopped working in May 2026 and are not
supported.

## Azure

`azure` speaks Azure Resource Manager over `net/http`, by hand. No SDK: the surface used is four
endpoints, and the taste matches reading Docker over its unix socket.

**Reader on the resource group is the whole requirement, and it covers cost too.** The Cost
Management query is an HTTP POST, which reads as a write and does not. Its operation is
`Microsoft.CostManagement/query/read`, so no Contributor role and no separate billing role is
needed. This is the least obvious fact in the lot.

Three token shapes are handled because three issuers disagree. Entra returns `expires_in` as a JSON
number; IMDS returns it as a string; App Service sends only `expires_on`. All three are parsed, and
a token is refreshed five minutes before it expires.

**Inventory takes two passes.** The catalogue call lists what exists but carries no running state,
so a second call per provider fills it in. A resource whose state was never read keeps `state` at
`NULL` — never the string `unknown`, and never `stopped`.

**The sweep is all or nothing.** `ReplaceAzureInventory` upserts and soft-deletes in one
transaction, and the hub calls it only when every call in the sweep succeeded. A partial read would
be swept as a batch of deletions, and "I cannot see Azure" would render as "the sandbox is empty".
A failed sync therefore changes no row; it writes its reason into `azure_sync`, which the page shows
in red above the last good table.

**Cost and inventory are separate facts.** `azure_costs` has no foreign key to `azure_resources`: a
resource deleted mid-month still cost money, and dropping its row would understate the bill. The API
joins the two for display only, and a cost row with no inventory row is shown struck through rather
than hidden. A resource Azure has not billed has `cost` at `null`, never `0`.

The cost query's columns are located **by name**, not by position: Azure orders them as it likes,
and reading `rows[0][1]` as a resource id works until the day it is the currency.

Configuration lives in `settings`, editable from the browser. At least one resource group is
required — reading a whole subscription needs a subscription-scope role assignment this hub does not
ask for. The client secret never leaves the API: reads expose `client_secret_set` only, and it never
appears in an error message or a log.

**Nothing here has read or acted on a real subscription.** Every Azure endpoint in the suite is an `httptest`
server. The tenant forbids creating an app registration and assigning a role, so the credential does
not exist until an administrator acts once. What *has* been checked against real Azure is the error
path: a deliberately wrong tenant id returns `AADSTS900021` and that message reaches the browser
intact, trace id included.


### Actions

Lot 4 adds three: start, stop and restart, on `Microsoft.Web/sites`. The sandbox holds no virtual
machine, so VM actions would have shipped untestable; the type-to-verb mapping is a table, and lot 5
adds VMs as a row rather than as a rewrite.

**Reader is no longer enough.** Every action is `Microsoft.Web/sites/start/action` or a sibling, so
acting needs **Website Contributor** on top of Reader. Both are asked for at once: a second round
trip through a tenant administrator is a second wait nobody controls.

**The action row is written before the call**, as a delivery is. **Unlike a delivery, it is never
replayed.** Rows left `pending` or `running` when the hub stops become `interrupted` at the next
start, and stay there. A lost alert is worse than a duplicate one, so deliveries replay; re-firing a
stop could stop a resource restarted by hand in the meantime, so actions do not. The honest record
is "nobody knows", and the next inventory sync says what actually happened.

**An action retries throttling only.** The read path retries 429 *and* 5xx, which is right for a
`GET`. A 500 on a `POST …/stop` may land after the stop happened, so repeating it is the same
double-action that the interrupted rule refuses, only faster. The retry policy is therefore passed
per call rather than held in the client's options: one `azure.Client` is shared by both sweeps and
the actions, and mutating its options for one call would race a sweep in flight.

**The state afterwards is read back, never inferred.** A successful stop is followed by one typed
`GET` on the resource, and whatever it returns is what the inventory stores — `Running` included, if
Azure has not caught up. A read that fails leaves the previous state untouched and `state_after`
`NULL`. Writing "Stopped" because a stop was asked for would break the invariant from the inside.

**The action URL is built from `arm_id`, not from `id`.** `NormalizeID` lowercases ids so inventory
and cost rows join; ARM's own casing is kept alongside, because building the request path from the
lowercased form would be a bet on every ARM path segment being case-insensitive — a bet that could
only be settled on the first real action, which is the worst possible moment.

The id travels in the request body rather than the path: an ARM id is mostly slashes, a Go 1.22
`ServeMux` wildcard does not match one, and percent-encoding it would tie the route to when
`net/http` unescapes a path.

## The hub process

`Run` starts the dispatcher, replays pending deliveries, then ticks:

| Every | Does |
|---|---|
| sampling interval | Marks hosts stale; an offline transition evaluates immediately rather than waiting. |
| minute | Evaluates the rules, persists transitions, publishes them, hands them to the dispatcher. |
| hour | Aggregates, purges samples, sessions and settled deliveries. |
| Azure inventory interval | Sweeps the configured resource groups. |
| Azure cost interval | Runs the month-to-date query. |

Channel and Azure configuration are held in an `atomic.Pointer`: the settings handler swaps the whole
thing, the evaluator only ever reads it.

**Saving the Azure settings syncs now, through a one-slot channel the run loop selects on.** Without
it the page shows "no inventory has run yet" beside an empty table until the next tick — and longer,
because the tickers were built at boot with the one-hour cadence an off integration uses and nothing
resets them until they first fire. The kick runs both syncs and realigns both tickers. Every
outcome is broadcast, failures included: an Azure read can take minutes to time out, and a page told
only about successes sits on "nothing has run yet" for the whole of a failure.

**Both sweeps run off the loop, one goroutine each, guarded by an `atomic.Bool` per scope.** Held
inline they would stop the hub evaluating rules for minutes, and a ticker buffers one tick, so those
minutes are dropped rather than caught up. The guard matters as much as the goroutine: two
overlapping sweeps let the stale view of whichever finishes last mark the other's fresh rows
deleted, which is the very thing the all-or-nothing transaction exists to prevent.

## The web layer

REST under `/api/v1`, cookie session, Argon2id password, a rate limiter on login. Live updates are
Server-Sent Events on `/api/v1/events`, one-way and therefore trivially proxyable, rather than a
second WebSocket.

The front-end is SvelteKit 5 with `adapter-static`, built to plain files and embedded with
`go:embed` from `internal/hub/webdist/build`, never from `web/`, so `go vet ./...` does not walk
`node_modules`. Charts are `uplot`. One binary serves the API and the interface.

## Configuration

Hub, read once at startup:

| Variable | Default | Meaning |
|---|---|---|
| `SM_LISTEN` | `:8090` | Listen address. |
| `SM_DATA_DIR` | `./data` | Where the SQLite file lives. |
| `SM_SECURE_COOKIES` | `false` | Set the Secure flag; turn on behind TLS. |
| `SM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

Agent, flags or environment:

| Variable | Flag | Meaning |
|---|---|---|
| `SM_HUB` | `--hub` | Hub URL, `wss://hub.example`. |
| `SM_TOKEN` | `--token` | The host token. |
| `SM_INSECURE` | `--insecure` | Allow plain `ws://` to a remote host. |
| `SM_DOCKER_SOCKET` | `--docker-socket` | Default `/var/run/docker.sock`; empty disables Docker. |

Everything else — sampling interval, retentions, alert rules, notification channels — is in the
database and editable from the interface.

## What is deliberately absent

- **No TLS of its own.** A reverse proxy does it better. `SM_SECURE_COOKIES=true` behind it.
- **No user accounts.** One local admin. Entra ID is deferred.
- **No per-rule routing.** Every enabled channel receives every transition. Mute a host to silence it.
- **No clustering.** One hub, one SQLite file, one machine.

## Testing

251 Go tests across 13 packages and 45 front-end tests, plus an end-to-end test that runs a real hub
and a real agent over a real WebSocket and asserts that an alert reaches a webhook and that the
delivery is recorded.

Two habits are worth keeping, because both caught defects the suite did not:

- **Verify against a running hub by hand.** Six of lot 1's defects and two of lot 2's came from
  manual checks: the exit code on a busy port, mount noise, a sensor legend taller than its chart,
  clipped axis labels, a unix socket path over the macOS 104-byte limit, phantom firing alerts, and a
  test notification linking to a host that does not exist.
- **Mutate the code to check the test.** A settings test passed against a handler that wrote before
  validating, because map iteration order let a later case overwrite the key it checked. A fan-out
  test was missing entirely, so a `break` where the loop has `continue` passed 174 tests.

Lot 3 repeated both lessons. Running a real hub found that saving the Azure settings synced nothing
for an hour, which no test covered because every test called the sync function directly. Mutation
found that a cost total repeated the hub's single budget beside each currency, and that a failed sync
was never broadcast — both of which the suite had been green on.

Lot 3 repeated both lessons. Running a real hub found that saving the Azure settings synced nothing
for an hour, which no test covered because every test called the sync function directly. Mutation
found that a cost total repeated the hub's single budget beside each currency, and that a failed sync
was never broadcast — both of which the suite had been green on.
