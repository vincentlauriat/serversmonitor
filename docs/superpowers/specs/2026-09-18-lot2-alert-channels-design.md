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

Three properties, and the order they are written in is the order they must hold.

**The row exists before the first attempt.** `Hub.evaluate` persists the event, then inserts one
`pending` delivery row per enabled channel, *then* hands the work to the queue. Only the worker moves
a row to `sent` or `failed`. Writing the row after a successful send would lose every delivery a
crash interrupts — silently, which is exactly the "was I told?" question §5 exists to answer. This
ordering is what makes the durable queue of §5 and the in-memory queue here the same queue.

**Never block the evaluator.** `Hub.evaluate` runs on the minute tick. An SMTP server that hangs for
30 s must not delay the next evaluation, and must not delay the *other* channels. Handing off to the
queue is a non-blocking send; the worker owns every network call.

**Never lose an alert to a transient failure.** A webhook that returns 503 because the receiver is
restarting is the normal case, not the exception. Each delivery is retried with backoff — 4
attempts at roughly 1 s, 5 s, 25 s — then marked `failed` with the last error kept.

Teams throttles above four requests per second and answers `429`. A `429`, and any `5xx`, is
retried. A `4xx` that is not `429` is not: a malformed card or a revoked URL will fail identically
forever, and retrying it only delays the alerts queued behind it.

**When the queue is full, a `resolved` is dropped last.** The plain reading — drop the oldest, keep
the newest — is right for a threshold that keeps re-firing, and wrong for a recovery: drop the
`resolved` and the last thing Vincent ever heard about a host is that it broke. So the queue drops
the oldest `fired` it holds; only if it holds nothing but `resolved` does it drop the oldest of
those. Either way the row stays in the table, moved to `failed` with the reason `queue full`, so a
drop is never silent.

## 5. Deliveries are recorded

A `pending` row is written before the first attempt, as §4 requires; everything below follows from
that.

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

**Teams.** A POST of an Adaptive Card to a Power Automate *workflow* webhook URL. Microsoft
disabled Office 365 connectors in Teams between 18 and 22 May 2026, so the old `webhook.office.com`
URLs are dead; workflow URLs live on `api.powerautomate.com`, `api.powerplatform.com` or
`flow.microsoft.com`. Verified against Microsoft Learn on 2026-09-18, the body is:

```json
{
  "type": "message",
  "attachments": [{
    "contentType": "application/vnd.microsoft.card.adaptive",
    "contentUrl": null,
    "content": {
      "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
      "type": "AdaptiveCard",
      "version": "1.2",
      "body": [ { "type": "TextBlock", "text": "…" } ]
    }
  }]
}
```

Two published limits shape the code: a message caps at **28 KB**, and more than **four requests per
second** is throttled with `429`. Neither is reachable by this hub in normal use, but the 28 KB cap
is why the card carries a summary and a link rather than a dump of the sample.

**Teams cannot be verified locally.** SMTP has a catcher and the webhook has `httptest`; a workflow
URL belongs to a real tenant. Lot 2 therefore ships Teams verified only against the payload shape
above, asserted byte for byte in a test. The first delivery to a real channel is Vincent's to
confirm, and the plan says so rather than letting a green test imply more than it proves.

## 7. Configuration lives in the database, not the environment

Lot 1 put everything in environment variables. Channels break that rule on purpose: Vincent will
want to change a recipient or test a webhook without restarting a container. Channel settings are
rows in `settings`, edited from a new **Notifications** tab, and each channel has an **enabled**
switch and a **Send a test** button.

The SMTP password is the one secret. It is stored in the database like the rest — the database
already holds the host tokens and the admin password hash, so it is already the trust boundary —
and it is **never returned by the API**. A read returns `smtp_password_set: true|false` and no
value at all. A write carries `smtp_password` only when it is being changed; the field absent from
the request body means *leave it alone*, and an empty string means *clear it*. Sniffing a magic
placeholder string out of the submitted value was the alternative, and it makes that string
impossible to use as a real password — a small trap with no upside over one boolean.

## 8. What lot 2 does not do

- **No per-rule routing.** Every enabled channel gets every alert. Routing by severity or by host is
  a real want, but it needs a severity model the hub does not have, and guessing one now would fix
  the wrong shape.
- **No digest or grouping.** Ten hosts going offline at once send ten messages. Grouping needs a
  window, and a window delays the first message, which is the one that matters.
- **No silence windows.** Muting a host already exists and covers the night-shift case.
- **No Slack or Discord app.** The generic webhook reaches both.
- **No delivery of alerts that predate the channel.** Enabling a channel does not replay history.

## 9. Errors

A channel that is disabled is skipped without a row. A channel enabled but misconfigured produces a
`failed` delivery with the error text, visible in Notifications — never a crash, never a retry loop
that outlives the alert. A `SM_PUBLIC_URL` that is not a URL is refused at save time, not at send
time.
