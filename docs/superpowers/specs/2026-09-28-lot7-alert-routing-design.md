# ServersMonitor — Lot 7: alert routing per rule

**Date**: 2026-09-28
**Status**: design, choices made by Vincent on 2026-09-28
**Scope**: this repository. Lot 2 made alerts arrive; lot 7 decides where each one arrives.

## 1. What is missing

The README says it plainly: *every enabled channel receives every transition*. A disk at 91 % on a
test VM and a production host going offline land in the same Teams channel and the same inbox. The
only lever today is muting a host, which silences it everywhere, including the dashboard.

## 2. The choices

Three questions were put to Vincent; the answers are the design.

| Question | Answer |
|---|---|
| What does a rule with no explicit routing receive? | **Every enabled channel.** Existing rules behave exactly as before the migration. |
| Where is the offline rule routed, and the cost guardrails? | **Two dedicated settings**, under Settings → Notifications, same picker as a rule. |
| Where does a `resolved` go if the routing changed while the alert was firing? | **Where the `fired` went, plus the current route.** Nobody is left with an alert that never ends. |

## 3. Data model (migration 7)

```sql
ALTER TABLE alert_rules ADD COLUMN channels TEXT;  -- NULL = every enabled channel
```

A route has three readable states, and the column keeps all three apart:

| Stored | Meaning |
|---|---|
| `NULL` | every enabled channel, now and when a new one is enabled later |
| `''` | no channel: the alert shows in the dashboard and notifies nobody |
| `'smtp,teams'` | exactly these, when they are enabled |

"No channel" is a real answer, different from muting: a muted host is not evaluated at all, a rule
routed nowhere still fires, still shows, still sits in the event log.

The two routes without a row live in `settings`: `notify_route_offline` and
`notify_route_guardrails`, same encoding, with `*` for "every channel" because a setting cannot be
NULL. An absent key reads as `*`.

A route naming a channel that is not enabled is kept, not rejected: enabling SMTP later must not
require editing every rule again. That channel is simply skipped while it is off, and the interface
says so next to the rule.

## 4. Where it hooks

`Hub.notify` and `Hub.notifyGuardrails` already build one delivery per enabled channel. They now
build one per channel in the event's route. Nothing else moves: the dispatcher, the retries, the
replay at boot and the `deliveries` table are unchanged, because a delivery row already names its
channel.

For a `fired`, the route is the rule's (or the offline setting, or the guardrail setting).

For a `resolved`, the route is the union of:

1. the channels that got a delivery row for the matching `fired` — the last `fired` of the same
   (rule, host), or of the same (subject, rule, detail) for a guardrail, older than this event;
2. the current route.

Any delivery row counts, whatever its state: a `fired` that failed on Teams still owes Teams a
`resolved`, because a person may have seen the Teams retry later or will see the failure in the
delivery log. The union is then filtered by what is enabled now: a channel turned off cannot
deliver anything.

If the `fired`'s deliveries were purged (30 days), only the current route applies. That is the
only degraded case and it concerns alerts older than a month.

A rule deleted while firing never resolves (unchanged lot 1 behaviour); its routing does not
matter. A `fired` whose rule has gone by the time the message is built uses every channel, as the
message already falls back to a zero threshold.

## 5. API

| Endpoint | Change |
|---|---|
| `GET /api/v1/alerts` | each rule gains `channels`: `null` or an array |
| `POST`/`PUT /api/v1/alerts/rules` | accept `channels`; absent or `null` means every channel |
| `GET`/`PUT /api/v1/notifications` | gain `offline_channels` and `guardrail_channels`, same shape |

A name other than `smtp`, `webhook`, `teams` is a 400. Duplicates are dropped, and the order is
normalised to the channel order, so a route reads the same everywhere.

## 6. Interface

- **Alert rules table**: a *Notify* column. `All channels`, `None`, or the channel names, with
  `(off)` after a channel that is not enabled.
- **Rule editor**: *Notify* with two choices, *every enabled channel* or *only these*, followed by
  three checkboxes. *Only these* with nothing ticked is *None*, said in words under the boxes.
- **Settings → Notifications**: a *Routing* block with the same picker for *Host offline* and
  *Azure guardrails*.

## 7. Tests that must exist

- a rule routed to Teams only produces one delivery, on Teams;
- a rule routed to nothing produces an event and no delivery;
- `NULL` and `''` survive a round trip through the store as two different answers;
- a `fired` sent to SMTP, then the rule re-routed to Teams: the `resolved` goes to SMTP and Teams;
- the same for a guardrail, and for the offline rule through its setting;
- a route naming a disabled channel produces nothing on it and no error;
- the API refuses an unknown channel and normalises order and duplicates.

## 8. Out of scope

Per-host routes (a rule scoped to one host already gives that). Several instances of one channel
type, such as two Teams channels or two recipient lists: channels stay three singletons. Quiet
hours, escalation, and repeat reminders for a long-firing alert.
