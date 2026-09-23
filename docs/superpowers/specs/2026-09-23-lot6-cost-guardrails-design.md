# Lot 6 — Cost guardrails: budget alerts, orphans, stop schedules

Design validated section by section with Vincent on 2026-09-23. Lots 1 to 5 are in `main` and
proven against the real subscription (2026-09-22: inventory and cost sync, a real `restart`, a VM
created with the agent installed by cloud-init, then deleted). This lot is the last of the six
planned in the lot 1 spec, and it adds no new credential: everything below runs on the three roles
the hub already holds (Reader, Website Contributor, Virtual Machine Contributor on
`rg-dev-vincent-sandbox`).

The three guardrails carry the same weight. None is a stretch goal.

## 0. Decisions, and what they rule out

| Decision | Why | What it rules out |
|---|---|---|
| Guardrail alerts travel through the lot 2 dispatcher, as transitions in an append-only log | One delivery pipeline; the channels already retry, log and link | A separate notification path for guardrails |
| Schedules emit lot 4 actions, marked `origin=schedule` | One action log, one state re-read, one set of failure rules | A scheduler that talks to ARM on its own |
| Orphans are a fact of the inventory, recalculated on every sweep | A separate table would drift from what Azure says | A curated list of orphans |
| Nothing is deleted without a person typing its name | Lot 5 rule, unchanged | Automatic deletion, however old the orphan |
| One time zone for the whole hub, `Europe/Paris` by default | Two resources in two zones is a table nobody reads right | Per-schedule zones |
| A schedule acts only when a boundary is crossed | The hub never fights a person who switched a machine on by hand | Continuous enforcement of the "off" state |
| One monthly budget for the hub, all currencies summed | Lot 3 decision; a budget per currency claims two budgets | Per-group or per-currency budgets |

Approaches considered and rejected: a separate "guardrails" engine with its own notifications and
journal (duplicates delivery and state re-read, two logs to show); delegating budgets to Azure
Budgets and Action Groups (needs one more role from the tenant admin, no per-resource share, and
covers neither orphans nor schedules).

## 1. Data model (migration 6)

**`azure_schedules`** — one row per scheduled resource.

| column | meaning |
|---|---|
| `resource_id` | lowercased ARM id, primary key, as in `azure_resources.id` |
| `off_windows` | JSON list of `{days:[1..7], from:"HH:MM", to:"HH:MM"}`; `to` before `from` means the window crosses midnight; days use ISO numbering (1 = Monday) |
| `enabled` | 0/1 |
| `last_boundary` | the last boundary the hub acted on, RFC 3339 in UTC; NULL until the first one |
| `last_applied_at` | when that action was issued |
| `created_at`, `updated_at` | |

Windows describe the **off** state. Outside every window the resource should be on. Windows of one
resource are merged at save time; a window covering all of every day is accepted (off until further
notice) and the form says so.

**`azure_guardrail_events`** — the counterpart of `alert_events` for what has no host. Append-only;
only transitions are written, exactly as in lot 1.

| column | meaning |
|---|---|
| `id` | |
| `subject` | `budget`, or the lowercased ARM id of a resource |
| `rule` | `budget_threshold`, `budget_projection`, `resource_share`, `orphan`, `schedule_failed` |
| `detail` | the threshold for `budget_threshold` (`80`, `100`), else `''` |
| `kind` | `fired` \| `resolved` |
| `value` | the figure the rule fired on (spent, projected, share, days silent, or 0) |
| `at` | RFC 3339 UTC |

Index on `(subject, rule, detail, id)`: the evaluator reads the last event of each rule instance to
decide whether the state changed.

**`azure_resources`** gains two columns: `orphan_reason` (text, `''` = not an orphan; one of
`disk_unattached`, `ip_unassociated`, `nic_without_vm`, `plan_without_site`, `hub_vm_silent`,
`unverified`) and `orphan_since` (RFC 3339, NULL when not an orphan). `unverified` is a reason in
its own right: the typed read answered 403, so the hub neither affirms nor denies (see §2).

**`azure_actions`** gains `origin` (`user` \| `schedule`, default `user`) and accepts the verb
`deallocate`; the type → verb table gains `Microsoft.Compute/virtualMachines` with
`start`, `deallocate`, `restart` (a VM's `stop` keeps billing the compute, so schedules never use it;
the page offers `deallocate` for VMs where it offers `stop` for sites).

**Settings** (in `settings`, editable without restart, like every Azure setting):

| key | default | meaning |
|---|---|---|
| `azure_budget_thresholds` | `80,100` | percentages of the monthly budget that each fire once |
| `azure_resource_share_pct` | `30` | a single resource above this share of the budget fires |
| `azure_hub_vm_silent_days` | `3` | a hub-created VM whose host is silent this long is an orphan |
| `azure_timezone` | `Europe/Paris` | the zone every schedule window is written in |

Refused at save time: a threshold outside 1–500, a share outside 1–100, a zone `time.LoadLocation`
does not know (the message names it), a schedule on a resource type absent from the type → verb
table.

## 2. Evaluator: budget and orphans

A package `internal/hub/guardrails` with no state of its own. It reads inventory, costs and
settings, computes the wanted state of every rule instance, compares it with the last event
journaled for that instance, and writes only the transitions. It runs at the end of every
**successful** cost sync (budget rules) and inventory sync (orphan rules). It never runs after a
failed sync: "silence is not zero", so a month with no cost data cannot resolve into "budget
respected". The lot 3 red banner over the last good table is what the page shows meanwhile.

### Budget — three rules on the month-to-date total

All currencies are summed. When two currencies appear, the message says so and shows the sum; a
budget per currency is a lot 3 non-goal.

- **`budget_threshold`**, one instance per configured percentage. Fires when spent ≥ threshold ×
  budget. Resolves when spent < threshold × budget, which in practice happens once, on the first
  of the month: every threshold resolves and the message says "new month". No hysteresis: the spent
  figure only grows within a month.
- **`budget_projection`**: `projected = spent × days_in_month / days_elapsed`, where
  `days_elapsed` counts the days Azure has actually billed — the cost `as_of` date, not today,
  because Cost Management lags a day and counting today would understate the pace by a full day
  early in the month. Not evaluated before the 4th billed day: three days of data project noise.
  Fires when projected > budget × 1.05, resolves when projected < budget × 0.95. The 5 % band keeps
  it from firing and resolving every hour around the budget.
- **`resource_share`**, one instance per resource: fires when that resource's month-to-date cost
  > `azure_resource_share_pct` % of the budget, resolves when it drops below (again, the first of
  the month, or when the budget setting rises). Deleted-but-billed resources count: the cost is a
  separate fact (lot 3), and the VM deleted yesterday must stay in the alert it caused.

A budget of 0 disables all three rules and resolves anything firing.

### Orphans — five detectors

Each is a typed read added to the inventory sweep, because the generic catalogue carries
`properties: null` (lot 3 finding):

| reason | type | typed read | condition |
|---|---|---|---|
| `disk_unattached` | `Microsoft.Compute/disks` | `GET …?api-version=2024-03-02` | `properties.diskState == "Unattached"` |
| `ip_unassociated` | `Microsoft.Network/publicIPAddresses` | `GET` (network API version already in use) | `properties.ipConfiguration` absent |
| `nic_without_vm` | `Microsoft.Network/networkInterfaces` | `GET` | `properties.virtualMachine` absent |
| `plan_without_site` | `Microsoft.Web/serverfarms` | `GET` | `properties.numberOfSites == 0` |
| `hub_vm_silent` | `Microsoft.Compute/virtualMachines` tagged `createdBy=ServersMonitor` | none (hub data) | the host named by `smHostId` is `never_seen`, or offline for more than `azure_hub_vm_silent_days` |

`orphan_since` is set at first detection and kept while the reason persists; it clears when the
reason goes away or the resource is deleted. The `orphan` event fires once per resource and
resolves the same way. A typed read that answers **403** marks the resource `unverified` and fires
nothing: the page shows "not verified", never "healthy". A **404** means the resource vanished
between the catalogue pass and the typed pass; it is skipped this sweep and the next sweep will
not list it.

### Delivery

Every transition becomes a `notify.Message` with the subject in place of the host name (the
resource name, or "Budget"), the rule as the metric, and a link to the Azure page. The channels
need no change. The at-least-once rule of lot 2 applies unchanged.

## 3. Stop schedules

- **Wanted state, not commands.** A schedule is a list of "off" windows in the hub's zone; the
  question "what should this resource be right now?" is a pure function of the windows and a
  `time.Time`, testable without a real clock.
- **Boundary crossing only.** The hub's minute ticker asks the evaluator, for every enabled
  schedule: is the most recent boundary (window start or end) more recent than `last_boundary`?
  If so, exactly one action is issued through the lot 4 path — `stop` (sites) or `deallocate`
  (VMs) at a window start, `start` at a window end — with `origin=schedule`. `last_boundary` is
  advanced **before** the call: a crash between the two never replays the action, the same
  principle as lot 4's `interrupted`. A resource switched on by hand inside its window stays on
  until the next boundary; the hub never fights a person.
- **Bounded catch-up at startup.** If the hub was down at a boundary, it applies that boundary on
  startup when it is less than 12 hours old, and otherwise ignores it and logs "missed boundary".
  Restarting on Monday morning a machine whose Sunday-night boundary was missed is useful;
  replaying yesterday's stop on a machine someone is using is not.
- **No retry.** A scheduled action that fails (429 budget exhausted, 5xx, state read back
  unchanged) stays failed in the action log, a `schedule_failed` event fires for that resource
  (resolved by the next successful scheduled action on it), and the next boundary tries again.
- **Zone and daylight saving.** Boundaries are computed in the configured `time.Location`. A
  boundary that falls into the hour skipped in spring moves to the next hour; a boundary in the
  hour repeated in autumn is applied once. Two dedicated tests.
- **Schedules and manual actions share the in-flight lock** of `azure_actions`: a scheduled stop
  on a resource with a manual action running is skipped this boundary, logged, and not retried.

## 4. API and page

All routes sit behind the session, like every Azure route.

| route | purpose |
|---|---|
| `GET /api/v1/azure/guardrails` | one answer: spent, budget, projection (or null before day 4), thresholds with their state, resources above the share, orphans (reason, since, month cost, `unverified`), the last 20 guardrail events |
| `GET`/`PUT /api/v1/azure/guardrails/settings` | the four settings of §1, validated at save |
| `GET /api/v1/azure/schedules` | every schedule with its windows and `last_boundary` |
| `PUT /api/v1/azure/schedules` | body `{resource_id, off_windows, enabled}`; the id travels in the body, never in the path (an ARM id is mostly slashes, lot 4 rule) |
| `POST /api/v1/azure/schedules/delete` | body `{resource_id}` |
| `POST /api/v1/azure/orphans/delete` | body `{resource_id, confirm_name}`; the lot 5 delete path generalised to an ARM id and type; refuses a wrong name, and refuses a type the role cannot delete (public IP) with a sentence rather than Azure's bare 403 |

The existing SSE `azure` event is republished after every evaluation; the page reloads the full
guardrails state (subscriptions attach themselves since lot 4).

**Page.** No new page: everything that costs money is already on the Azure page. Three blocks
under the resource table:

- **Budget.** The existing bar gains the threshold marks, the end-of-month projection as a
  figure, and a plain red when any budget rule fires. Below it, the resources above the share.
- **Orphans.** reason / resource / since / month cost / *Delete*, which opens the same
  confirm-by-name modal as lot 5. An `unverified` row has no button.
- **Schedules.** A "schedule" column in the resource table (icon when enabled, windows on hover)
  and a per-resource editor: seven days × from/to, an "evenings + weekends" preset that fills
  `Mon–Fri 20:00–07:00 + Sat, Sun`. The recent-actions table shows `origin`.
- **Settings → Azure** gains a "Guardrails" block with the four settings.

## 5. Errors

| case | rule |
|---|---|
| sync failed | no evaluation, no transition; the previous state stays under the lot 3 banner |
| typed read 403 | `unverified`, neither healthy nor orphan, no event |
| typed read 404 | skipped this sweep |
| scheduled action failed | action log + `schedule_failed` event; no retry; next boundary tries |
| zone unknown at save | refused with the name; at runtime (cannot happen with `time/tzdata` embedded) fall back to UTC with a log line and a page warning |
| orphan delete refused by Azure | Azure's message, intact, in the modal; the row stays |
| manual action in flight at a boundary | boundary skipped and logged, not retried |

## 6. What tests can prove, and what they cannot

Three proofs per task, as in lots 3–5: unit, mutation (break the code, watch the test fail, restore),
then real.

- **Budget evaluator**: a table of (billed days, spent, budget, previous events) → expected
  transitions; hysteresis both ways; first of the month; two currencies; budget 0. Every case
  carries the previous journal, to prove that only transitions are written.
- **Orphan detectors**: one `httptest` per detector with Azure's real JSON shape (copied from an
  `az … show`), plus 403 and 404.
- **Schedules**: injected clock; crossing, not crossing, catch-up at 11:59 and 12:01 of age,
  spring and autumn boundaries, merged windows, 24×7 window, in-flight lock.
- **End to end**: a schedule on a fake site → `azure_actions` row with `origin=schedule` → state
  read back → a `schedule_failed` event when the fake answers 500.
- **Real** (needs Vincent's hand, as the classifier refuses these to Claude): a ten-minute window
  on `webapp-vlauriat-probe-09121238`, the stop and the start in the Azure activity log with the
  managed identity as caller; a disk created on purpose, detected as `disk_unattached`, deleted
  by name.

What tests cannot prove: that Azure's cost lag is one day and not two on a given day (the
projection reads `as_of` rather than assuming), and that a real 403 on a typed read looks like the
one the tests fake (the lot 3 error path is the only real 403 seen so far).

## 7. Out of scope, said explicitly

Automatic deletion (never). Budgets per group or per currency. Schedules on Container Apps (lot 4
does not stop them). Forecasting beyond the rule of three. Guardrail-specific notification
channels. Reading a subscription-wide budget (the roles are group-scoped).
