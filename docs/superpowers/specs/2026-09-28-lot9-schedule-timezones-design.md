# ServersMonitor — Lot 9: a time zone per stop schedule

**Date**: 2026-09-28
**Status**: design, small enough to need no open question
**Scope**: this repository. Lot 6 reads every stop schedule in one hub-wide zone.

## 1. What is missing

`azure_timezone` is one setting for the whole hub. A site that serves New York and a VM used from
Paris cannot both be off "in the evening".

## 2. The design

- **Migration 9**: `azure_schedules.timezone TEXT NOT NULL DEFAULT ''`. `''` means the hub-wide zone,
  **read at every tick rather than copied**, so every schedule that existed before keeps following
  the setting, including when it changes later.
- **Evaluation**: `runSchedules` and the schedules view take `guardrails.ScheduleLocation(own, hub)`.
  A zone that no longer loads falls back to the hub's, never to UTC: the hub's zone is at least one a
  person chose for schedules.
- **Validation**: `ValidateZone` accepts `''` or an IANA name, and refuses `Local`, which would mean
  the zone of whatever machine the hub runs on.
- **Boundaries**: unchanged. Changing a zone moves the boundaries; the loop acts only on a boundary
  newer than the last one it acted on, exactly as when the windows are edited.
- **API**: `timezone` (saved) and `effective_timezone` (what the windows are read in now) on
  `GET /api/v1/azure/schedules`; `timezone` on `PUT`.
- **Page**: a zone field in the schedule editor, suggestions from `Intl.supportedValuesOf`, and the
  hub zone as its placeholder; the clock icon's tooltip names the zone when it is the schedule's own.

## 3. Tests

A schedule in `America/New_York` on a hub in UTC starts at 16:30 New York and stops at 20:01 New York
(00:01 UTC the next day); store round trip and edit of the zone; API refusal of `Paris` and `Local`,
`effective_timezone` for both cases; the helpers on the front.

## 4. Out of scope

Windows in several zones inside one schedule. Showing each resource's local time on the page.
