# ServersMonitor — Lot 12: reminders and quiet hours

**Date**: 2026-10-18
**Status**: design, choices made by Vincent on 2026-10-18
**Scope**: this repository. Lot 7 decided where an alert goes; lot 12 decides when, and how often.

## 1. What is missing

An alert is said once. If nobody acts, it never comes back, however long the disk stays full. And
every channel speaks at any hour: a disk at 91 % at 3 a.m. lands in the same inbox as a host down.

## 2. The choices

| Question | Answer |
|---|---|
| Where is the reminder interval set? | **Per rule**, plus one each for the offline rule and the guardrails. Default never. |
| What happens to a notification during a channel's quiet hours? | **Held, then sent** when they end, in order. Nothing is lost. |
| Does a host going offline respect quiet hours? | **No**, it is sent at once, and so is its recovery. |

## 3. Data (migration 11)

`alert_rules.remind_minutes` (0 never) and `deliveries.reminder`. A reminder is a delivery of the
fired event it repeats, not a row of its own in `alert_events` or `azure_guardrail_events`: those
journals are read as "the last row is the current state" (`LastEventPerKey`, `Machine.Restore`, the
guardrail `transitions`), and a reminder there would make a firing alert stop looking like it
fires. As a delivery of the fired event it also counts for `FiredChannels`.

A delivery may be `held`. Not a schema change; four places learn it: purge keeps it, channel health
ignores it, the boot replay leaves it alone, the minute tick releases it.

## 4. Reminders

Every minute, for every (rule, host) whose last event is `fired`: the interval is the rule's (the
offline setting for rule 0); the rule must still exist (a rule deleted while firing never resolves,
lot 1), the host must exist and not be muted. Due when the last reminder, read from the deliveries,
or else the fire time, is at least one interval old. Routed like a resolved: the current route plus
where the fired went. Guardrails the same, with their setting. A reminder renders as an alarming
message, "still offline" or "(reminder)", with "Firing since"; the webhook gets `kind: reminder` and
`since`. A full queue drops reminders first.

## 5. Quiet hours

Per channel, the stop schedules' window list, read in the hub-wide zone (`azure_timezone`), shown
next to the fields. A transition owed to a quiet channel gets a `held` row, except the offline rule's.
Every minute, held rows of channels no longer quiet turn `pending` and are queued, rebuilt with the
event's own time; a channel switched off retires them as failed, "channel disabled". Reminders are
skipped, not held: the next tick after the quiet hours sends the one that is due. Windows covering
the whole week are refused.

## 6. Tests

Message: a reminder is alarming, red on Teams, titled "still offline" for rule 0, carries "Firing
since". Queue: a reminder goes before a fired. Store: held rows survive purge, are ignored by health,
are not pending until released, release once; the last reminder of an alert and of a guardrail; a
rule's interval round trip. Hub: one reminder per interval, none before, none twice in a minute;
never for no interval, a muted host, a deleted rule, a resolved alert; offline and guardrail
reminders on their routes; held then released after the window; offline bypasses; guardrails held;
reminders skipped while quiet then sent once; a disabled channel retires its held rows; a reminder
row rebuilds as a reminder. API: bounds on intervals, quiet hours round trip with the zone, the
whole week and unknown channels refused. Front: labels, the held state, the zone never sent back.
By hand: a real alert held during webhook quiet hours and released the minute they ended.

## 7. Out of scope

Escalation to another channel after N reminders. Quiet hours per rule. A digest of what was held.
