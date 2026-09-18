# ServersMonitor — Lot 2: alert channels

**Date**: 2026-09-18
**Status**: design, following the lot 1 decomposition Vincent approved on 2026-09-17
**Scope**: this repository. Lot 1 shipped alerts that are *visible*; lot 2 makes them *arrive*.

## 1. What is missing

Lot 1's README says it plainly: *no notifications; alerts show in the interface*. A hub you have to
look at does not tell you a machine died at 3 a.m. Vincent chose three channels during the
brainstorming: **SMTP**, a **generic webhook** (ntfy-compatible), and **Microsoft Teams**.

## 2. Where it hooks, and nowhere else

Lot 1's alert machine emits exactly two transitions, `pending→firing` and `firing→ok`, and those are
the only rows written to `alert_events`. **Lot 2 subscribes to those two events and nothing else.**

This is not an implementation convenience, it is the whole design. Anything that delivers on a
richer signal — every evaluation, every sample, a timer — re-derives state that the machine already
owns, and the two copies drift. The consequence to keep: a flapping metric produces no delivery at
all until it has breached for its full duration, because `pending→ok` is silent by construction.

`Hub.evaluate` already loops over the events it just persisted. That loop is the hook.

## 3. What a delivery is

One event becomes one message per enabled channel. A message carries:

| Field | Source |
|---|---|
| host name | `hosts.name` |
| metric, kind (`fired` / `resolved`), value | the event |
| threshold and duration | the rule, or the implicit offline rule |
| time | the event |
| a link to the host page | `SM_PUBLIC_URL` + `/hosts/{id}` |

**`SM_PUBLIC_URL` is new and required for links.** The hub cannot know the address a human types:
it may sit behind a reverse proxy, a tunnel, a port mapping. With it unset, messages carry no link
rather than a wrong one.

## 4. Delivery must never block or lose an alert

Two properties, in tension, and both required.

**Never block the evaluator.** `Hub.evaluate` runs on the minute tick and holds no lock a delivery
should wait on. An SMTP server that hangs for 30 s must not delay the next evaluation, and must not
delay the *other* channels. Deliveries therefore leave the evaluator through a buffered queue and
are sent by a worker.

**Never lose an alert to a transient failure.** A webhook that returns 503 because the receiver is
restarting is the normal case, not the exception. Each delivery is retried with backoff — 4
attempts, roughly 1 s, 5 s, 25 s — and then given up on, with the failure recorded.

When the queue is full (a hub that has been unable to reach anything for a long time), the **oldest
pending delivery is dropped, not the newest**: the newest alert is the one that matters, and the
drop is counted and logged rather than silent.

## 5. Deliveries are recorded

A new table, `deliveries`: one row per (event, channel), with its state (`pending`, `sent`,
`failed`), attempt count, last error and timestamps. Three reasons:

1. **The settings page can show whether a channel actually works.** "Configured" is not "working";
   a wrong SMTP password is invisible until the first real alert, which is the worst moment to find
   out.
2. **A hub restarted mid-delivery resumes.** Pending rows are re-queued at startup.
3. **It answers "was I told?"** after an incident, which is the question that matters.

Retention: deliveries older than the raw-sample retention are purged with everything else.

## 6. The three channels

Each implements one interface: given a message, either deliver it or return an error. No channel
knows about the others, about retries, or about the queue.

**SMTP.** Host, port, username, password, from, to (a list), TLS mode (`starttls`, `tls`, `none`).
Plain text body, no HTML: a monitoring alert is read on a phone, at night. The subject carries what
matters: `[ServersMonitor] mac-vincent memory 92% (fired)`.

**Webhook.** One URL, a `POST` of the message as JSON, optional headers (for an ntfy token). The
JSON is the message of §3, verbatim — ntfy renders it, Discord and Slack want their own shape and
get it through their own compatibility endpoints, which is their problem, not the hub's. A
`title`/`message`/`priority`/`tags` trio is included alongside, because ntfy uses exactly those and
it costs three fields.

**Teams.** A POST of an Adaptive Card to a workflow URL. Teams retired the old Office 365 connector
in 2025; the current path is a Power Automate *workflow* webhook that accepts an Adaptive Card
payload. The URL is configured the same way as the generic webhook; only the body shape differs.

## 7. Configuration lives in the database, not the environment

Lot 1 put everything in environment variables. Channels break that rule on purpose: Vincent will
want to change a recipient or test a webhook without restarting a container. Channel settings are
rows in `settings`, edited from a new **Notifications** tab, and each channel has an **enabled**
switch and a **Send a test** button.

The SMTP password is the one secret. It is stored in the database like the rest — the database
already holds the host tokens and the admin password hash, so it is already the trust boundary —
and it is **never returned by the API**: reads send back a placeholder, and a write that contains
the placeholder leaves the stored value alone.

## 8. What lot 2 does not do

- **No per-rule routing.** Every enabled channel gets every alert. Routing by severity or by host is
  a real want, but it needs a severity model the hub does not have, and guessing one now would fix
  the wrong shape.
- **No digest or grouping.** Ten hosts going offline at once send ten messages. Grouping needs a
  window, and a window delays the first message, which is the one that matters.
- **No silence windows.** Muting a host already exists and covers the night-shift case.
- **No Slack or Discord app.** The generic webhook reaches both.

## 9. Errors

A channel that is disabled is skipped without a row. A channel enabled but misconfigured produces a
`failed` delivery with the error text, visible in Notifications — never a crash, never a retry loop
that outlives the alert. A `SM_PUBLIC_URL` that is not a URL is refused at save time, not at send
time.
