# Lot 4 — Azure actions: start, stop, restart

**Goal.** From the Azure page, act on a resource the hub already shows: start it, stop it, restart
it. Every action leaves a trace, and the state shown afterwards is read back from Azure, never
guessed.

**Out of scope.** Creating or deleting resources is lot 5. Acting automatically on cost — stopping
something because it is expensive — is lot 6. This lot only ever does what Vincent clicked.

---

## 1. What this lot acts on, and why it is not VMs

The sandbox contains **no virtual machine**. Verified on 2026-09-18: 6 live resources, all of them
`Microsoft.Web/sites` (4) and `Microsoft.Web/serverfarms` (2). The first VM appears in lot 5.

So lot 4 acts on **App Services** (`Microsoft.Web/sites`), which is also the type lot 3 already
enriches with a real `state`. Building VM actions now would mean shipping a code path against a
resource type that does not exist yet and cannot be tested even after the credential unblocks.

The design stays open: which types are actionable, and what each action maps to, is one table in
one file. Adding `Microsoft.Compute/virtualMachines` in lot 5 is a row, not a redesign.

| Type | Action | ARM call | Answer |
|---|---|---|---|
| `Microsoft.Web/sites` | start | `POST …/start?api-version=2023-12-01` | 200, empty body |
| `Microsoft.Web/sites` | stop | `POST …/stop` | 200, empty body |
| `Microsoft.Web/sites` | restart | `POST …/restart` | 200, empty body |
| *(lot 5)* `…/virtualMachines` | start / deallocate / restart | `POST …/start`, `/deallocate`, `/restart` | **202**, `Azure-AsyncOperation` header |

That last line is why the action lifecycle below has a `running` state even though nothing in lot 4
returns 202 yet. It is not speculative machinery: it is the shape that lets lot 5 add VMs without
rewriting the table, the API and the page.

---

## 2. The credential problem gets worse, and must be said out loud

Lot 3 needs **Reader**. **Reader cannot start, stop or restart anything.** It grants `*/read`, and
every action here is `Microsoft.Web/sites/start/action` or its siblings.

The ask recorded in `TODOS.md` has been corrected in the same pass as this spec: the tenant
administrator is asked, **once**, for an identity holding **Reader + Website Contributor** on
`rg-dev-vincent-sandbox`. Website Contributor is the narrowest built-in role that covers the three
actions; Contributor would also work and grants far more. **Virtual Machine Contributor** is added
the day lot 5 creates a VM, not before.

Consequence for this lot, stated the same way lot 3 stated its own: **lot 4 ships fully tested
against a fake ARM and unable to act on anything real until a third party acts.** A 403 from ARM is
not a bug to hide — see §6.

---

## 3. An action is a record, written before the call

The lot 2 discipline applies: **the row exists before the attempt**, so a crash mid-flight leaves a
trace instead of silence.

```
azure_actions
  id            INTEGER PRIMARY KEY
  resource_id   TEXT NOT NULL      -- normalized, lowercase, as in azure_resources
  resource_name TEXT NOT NULL      -- copied, so the log survives the resource's deletion
  action        TEXT NOT NULL      -- start | stop | restart
  status        TEXT NOT NULL      -- pending | running | succeeded | failed | interrupted
  requested_at  TIMESTAMP NOT NULL
  finished_at   TIMESTAMP
  error         TEXT               -- the ARM code and message, intact
  state_before  TEXT               -- what the inventory said, may be NULL
  state_after   TEXT               -- read back from Azure, may be NULL
```

`resource_name` is copied rather than joined, deliberately, for the same reason `azure_costs` has no
foreign key: a resource deleted next week must not erase the record that it was stopped today.

**And here lot 2's rule is deliberately inverted: an interrupted action is never replayed.**
Deliveries replay at startup because a lost alert is worse than a duplicate. An action is the
opposite: re-firing `stop` at startup could stop a site Vincent restarted by hand in the meantime.
So on boot, any row left `pending` or `running` becomes **`interrupted`**, with `state_after` left
`NULL`, and the next inventory sync says what actually happened. The log reads "I do not know
whether this went through" — which is the truth — rather than a comfortable lie in either direction.

---

## 4. The API

```
POST   /api/v1/azure/actions   {"resource_id": "/subscriptions/…", "action": "stop"}
                               → 202 {"action_id": 17, …}
GET    /api/v1/azure/actions   → the log, most recent first
```

Behind `s.auth`, like every other mutating route; the session cookie is already `SameSite=Strict`.

- **The resource id travels in the body, not in the path.** An ARM id is
  `/subscriptions/…/resourceGroups/…/providers/…` — it is mostly slashes, and a `{id}` wildcard in
  Go 1.22's `ServeMux` does not match `/`. Percent-encoding it would make the route depend on when
  `net/http` unescapes the path, which is exactly the kind of thing that works in a test and fails
  behind a proxy. The body also makes `POST` and `GET /api/v1/azure/actions` the same path.
- **Scope is enforced by the server, not by the page.** A resource id outside the configured groups
  is refused with 404, whatever the page sent. The browser is not a security boundary.
- **A resource the hub has never inventoried is refused.** The hub does not act on an id it cannot
  show; that is how a typo becomes a 404 instead of an action on a stranger's production.
- **One action in flight per resource.** A second request while one is `pending` or `running`
  returns **409** with the running action's id. Without it, a double-click is a stop-then-start race
  whose outcome depends on ARM's ordering.
- The response is immediate (202); the call to Azure runs in the background. A restart takes tens of
  seconds, and an HTTP request held open that long dies in some proxy and leaves the browser
  believing the action failed when it succeeded.
- Progress reaches the page over the existing SSE channel (`GET /api/v1/events`), the one lot 2 and
  lot 3 already broadcast on. **Failures are broadcast too** — the defect found by hand in lot 3 was
  exactly a failure that nothing published.

---

## 5. The state shown afterwards is read, never assumed

When an action reports success, the hub does **one targeted enrichment read** of that resource
(`GET …/Microsoft.Web/sites/{name}?api-version=2023-12-01`) and writes the `state` it returns.

It does not write "Stopped" because it asked for a stop. That would be the lot 1 invariant broken
from the inside: **the inventory is what Azure said, not what we intended.** If the read fails,
`state_after` stays `NULL`, the resource keeps the state it had, and the page shows a dash — never a
wrong certainty.

An App Service also does not always reach the requested state instantly. The hub reads once, records
what it got, and lets the periodic sync correct it. It does not poll in a loop hoping for the answer
it wants.

---

## 6. Errors, shown intact

**Prerequisite, checked in the code and not assumed.** `armError` (`internal/hub/azure/client.go`)
flattens ARM's failure into `fmt.Errorf("%s: %s", http.StatusText(status), message)`. The HTTP
status survives as *text*, so the table below — which needs to branch on 403 versus 404 versus 409 —
cannot be written against today's client. **The first task of the plan is a typed error carrying
`Status int`**, with `armError` returning it and the existing `Retryable` marking left untouched.
Parsing `http.StatusText` back out of a message string would be the shell-grep mistake, in Go.

What changes next is that **403 is now the interesting error**, and it must not read as a bug in the
hub:

| ARM answer | Shown as |
|---|---|
| 403 | "The credential can read Azure but not act on it. The missing role is **Website Contributor** on the resource group." |
| 404 | "Azure no longer knows this resource." Followed by an inventory sync — it was probably deleted. |
| 409 | ARM's own conflict message, intact. Azure is already doing something to that resource. |
| 429 | Retried 1 s / 5 s / 25 s. Throttling means the request never reached the resource. |
| 5xx | **Not retried.** See below. |
| anything else | The ARM `code` and `message`, intact, as `AADSTS900021` was in lot 3. |

**An action does not inherit the read path's retry policy, and this is a deliberate departure.**
The lot 3 client retries 429 *and* 5xx, which is right for a `GET`: reading twice costs nothing. A
`POST …/stop` that answers 500 may well have stopped the site already, and sending it again is the
same double-action that §3 refuses to commit at startup — only faster. So actions retry **429 only**,
where Azure is saying it did not even look at the request.

A 5xx therefore ends the action as `failed`, and **§5's read-back is what settles what actually
happened**: the hub asks Azure for the resource's state and records the answer. "It failed" and
"it failed after doing it" are distinguishable by looking, not by guessing.

---

## 7. The page

On the Azure page, each actionable resource gets start / stop / restart, disabled while one is in
flight, and disabled entirely when Azure is off or the sync last failed.

- **Stop and restart ask for confirmation, naming the resource.** `sandboxmgr`, `visualrami` and
  `urbanexplorer` are sites that are up; a mis-click takes one offline. Start does not confirm —
  the worst case is a resource running that was not.
- A resource whose `state` is `NULL` still gets buttons. Not knowing the state is not a reason to
  forbid acting; it is a reason not to pretend the buttons know better.
- **The action log is visible on the page**, not hidden in a database. Twenty most recent, with
  their outcome and error. It is the answer to "did my stop go through?", including when the answer
  is "nobody knows, the hub was killed".

---

## 8. What tests can prove, and what they cannot

Provable here, against a fake ARM: the three actions and their URLs, the record written before the
call, one action per resource at a time, the 403/404/409 paths, **that a 5xx on an action is not
retried while a 429 is**, `interrupted` on restart, the read-back, scope enforcement, and the page in
each state.

**Not provable until a tenant administrator acts:** that a single real App Service ever starts or
stops from this code. Lot 3 shipped under the same honest caveat and it goes into
`TODOS.md` → "À prouver hors tests" the same way. This lot must not be described as verified against
Azure, in the README or anywhere else, until one real action has run.
