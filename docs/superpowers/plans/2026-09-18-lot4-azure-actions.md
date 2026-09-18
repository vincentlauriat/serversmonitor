# ServersMonitor Lot 4 (Azure actions) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** From the Azure page, start, stop or restart an App Service the hub already shows. Every action leaves a trace, and the state shown afterwards is read back from Azure, never guessed.

**Architecture:** `internal/hub/azure` gains an action table and two calls (`Do`, `ReadState`). The hub owns the lifecycle: it writes the record before the call, runs the call off the request, reads the state back, and publishes the outcome. `azure_actions` is the audit log the page reads.

**Tech Stack:** Go standard library only, as in lots 1-3. No Azure SDK.

**Spec:** `docs/superpowers/specs/2026-09-18-lot4-azure-actions-design.md`

## Global Constraints

- Branch `feat/lot4-azure-actions`; never commit to `main`. Conventional commits, English.
- **An action is written before it is attempted, and never replayed.** Rows left `pending` or `running` at startup become `interrupted`. This is the deliberate opposite of the lot 2 delivery rule; do not "fix" it.
- **The state after an action is read from Azure**, never inferred from the action requested. A read that fails leaves the state untouched and `state_after` `NULL`.
- **Actions retry on 429 only.** A 5xx on a `POST …/stop` may land after the stop happened.
- **Scope is enforced server-side.** An id outside the configured groups, or absent from `azure_resources`, is a 404 whatever the browser sent.
- Every task boundary leaves `go build ./...` green, `go vet ./...` clean, `svelte-check` at zero errors.
- No test may reach the network. Every Azure endpoint is an `httptest` server.

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/hub/azure/client.go` | *(modify)* typed `*Error` carrying `Status`, single-object `Get`, `PostAction` |
| `internal/hub/azure/actions.go` | *(new)* type → action table, `Do`, `ReadState` |
| `internal/hub/store/migrations/0004_actions.sql` | *(new)* `azure_actions` |
| `internal/hub/store/azure_actions.go` | *(new)* start / finish / interrupt / list |
| `internal/hub/azure_action.go` | *(new)* the hub's action lifecycle |
| `internal/hub/server/api_azure.go` | *(modify)* `POST` and `GET /api/v1/azure/actions` |
| `web/src/routes/azure/+page.svelte` | *(modify)* buttons, confirmation, action log |
| `web/src/lib/azure.ts` | *(modify)* pure helpers, unit-tested |

---

### Task 1: A typed ARM error, and a GET that returns one object

**Files:**
- Modify: `internal/hub/azure/client.go`
- Test: `internal/hub/azure/client_test.go`

**Why first.** Two things the rest of the lot needs do not exist, and both were verified in the code rather than assumed:

1. `armError` returns `fmt.Errorf("%s: %s", http.StatusText(status), …)`. **The HTTP status survives only as English text.** Task 5 has to branch on 403 versus 404 versus 409; recovering that by parsing `http.StatusText` back out of a message would be the `grep`-the-XML mistake, in Go.
2. `GetAll` unmarshals into `page{Value, NextLink}`. A single-resource response (`{"id":…,"properties":{…}}`) unmarshals **without error and yields zero items** — a silent empty, exactly the failure mode lot 1 forbids. The read-back in task 4 needs a `Get` that returns the body.

**Interfaces:**
```go
// Error is an ARM failure with its status kept as a number.
type Error struct {
    Status  int
    Code    string // ARM's own code, e.g. "AuthorizationFailed"
    Message string
}
func (e *Error) Error() string  // unchanged text: "Forbidden: <message>"
func StatusOf(err error) int    // 0 when the failure was not an HTTP answer

// Get returns one object's body. No pagination, no page envelope.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error)
```

- [ ] **Step 1: Write the failing tests**

In `client_test.go`:
- `TestErrorKeepsTheStatusAsANumber` — a 403 from the fake ARM, then `azure.StatusOf(err) == 403` and `errors.As(err, &*azure.Error)` finds `Code == "AuthorizationFailed"`.
- `TestStatusSurvivesTheRetryableWrapper` — a 429 is wrapped by `MarkRetryable`; `StatusOf` must still return 429. (`retryableError` has `Unwrap`, so `errors.As` traverses it — the test exists so a later refactor cannot quietly drop that.)
- `TestStatusOfANonHTTPFailureIsZero` — a connection refused gives `StatusOf(err) == 0`, not a misleading 500.
- `TestGetReturnsOneObjectNotAPage` — the fake serves `{"id":"/x","properties":{"state":"Running"}}`; `Get` returns those bytes. Then the counter-test that justifies this task: `GetAll` against the same handler returns **zero items and no error**, which is why `Get` exists.
- `TestErrorTextIsUnchanged` — the existing lot 3 expectations (`strings.Contains(err.Error(), "does not have authorization")`) still hold.

- [ ] **Step 2: Implement**

`armError` builds and returns `*Error`; its `Error()` keeps the exact previous format including `truncate(msg, 400)`. `azureError` (the token endpoint's flat envelope) is left alone. `Get` calls `c.do(ctx, http.MethodGet, c.url(path, query), nil)` and returns the body verbatim.

- [ ] **Step 3: Verify** — `go test ./internal/hub/azure/...`; the whole lot 3 suite still passes untouched.

---

### Task 2: The action call

**Files:**
- Create: `internal/hub/azure/actions.go`, `internal/hub/azure/actions_test.go`
- Modify: `internal/hub/azure/client.go` (the 429-only path)

**Interfaces:**
```go
type Action string
const (ActionStart Action = "start"; ActionStop Action = "stop"; ActionRestart Action = "restart")

// Actionable maps a resource type to the ARM verb for each action. A type that
// is absent is not actionable, and the API says so rather than guessing a URL.
var Actionable = map[string]map[Action]string{
    "microsoft.web/sites": {ActionStart: "start", ActionStop: "stop", ActionRestart: "restart"},
}

func Supports(resourceType string, a Action) bool
func Do(ctx context.Context, c *Client, resourceID string, a Action) error
func ReadState(ctx context.Context, c *Client, resourceID, resourceType string) (*string, error)
```

- [ ] **Step 1: Write the failing tests**

- `TestDoCallsTheRightURL` — for each of the three actions, the fake ARM records `r.Method`, `r.URL.Path` and the `api-version`. Expect `POST /subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/app/start?api-version=2023-12-01`. The path is built from the resource id, so **the id's own casing is preserved in the URL** while the stored id stays lowercased.
- `TestUnknownTypeIsNotActionable` — `Supports("microsoft.web/serverfarms", ActionStop)` is false, and `Do` on one returns an error naming the type without ever opening a connection. An App Service *Plan* is not a site; stopping one is not a thing.
- `TestA429IsRetried` — two 429s then a 200; the fake sees three calls, the waits are 1 s and 5 s.
- `TestA500IsNotRetried` — **the load-bearing test of this lot.** The fake counts calls; after a 500, `Do` returns an error and the fake has been called **once**. A comment in the test says why: the stop may already have happened.
- `TestA403IsNotRetriedAndKeepsItsStatus` — one call, `StatusOf(err) == 403`.
- `TestReadStateReturnsNilWhenAzureOmitsIt` — the enrichment read answers `{"id":"/x","properties":{}}`; `ReadState` returns `(nil, nil)`. Not knowing is not "Stopped".
- `TestReadStateFailureIsAnError` — a 500 on the read-back is an error, not a silent `nil`; task 4 must be able to tell them apart.

- [ ] **Step 2: Implement**

`Do` builds the path from the id, calls a client method that uses the **429-only** retry predicate, and discards the body (App Services answer 200 with nothing). `ReadState` calls `Get` on the typed provider path with `api-version=2023-12-01` and reads `properties.state`, reusing the shape already declared in `inventory.go` rather than a second copy.

For the retry policy, add to `Options` a predicate rather than a boolean — `Retry func(error) bool`, defaulting to `Retryable` — and pass the 429-only one from `Do`. A boolean named `noRetry5xx` would have to be re-read backwards at every call site.

- [ ] **Step 3: Verify** — `go test ./internal/hub/azure/...`.

---

### Task 3: Storage

**Files:**
- Create: `internal/hub/store/migrations/0004_actions.sql`, `internal/hub/store/azure_actions.go`
- Modify: `internal/hub/store/store_test.go` (schema version 3 → 4, and the table list)
- Test: `internal/hub/store/azure_actions_test.go`

**Interfaces:**
```go
type AzureAction struct {
    ID            int64
    ResourceID    string
    ResourceName  string
    Action        string
    Status        string // pending | running | succeeded | failed | interrupted
    RequestedAt   time.Time
    FinishedAt    *time.Time
    Error         string
    StateBefore   *string
    StateAfter    *string
}

// StartAzureAction writes the pending row. It returns ErrActionInFlight if this
// resource already has one pending or running.
func (s *Store) StartAzureAction(a AzureAction) (int64, error)
func (s *Store) MarkAzureActionRunning(id int64) error
func (s *Store) FinishAzureAction(id int64, status, errMsg string, stateAfter *string, now time.Time) error
func (s *Store) InterruptAzureActions(now time.Time) (int, error) // startup
func (s *Store) ListAzureActions(limit int) ([]AzureAction, error)
var ErrActionInFlight = errors.New("store: an action is already running on this resource")
```

- [ ] **Step 1: The migration**

```sql
CREATE TABLE azure_actions (
  id            INTEGER PRIMARY KEY,
  resource_id   TEXT NOT NULL,          -- lowercased, as in azure_resources
  resource_name TEXT NOT NULL,          -- copied on purpose, see below
  action        TEXT NOT NULL,          -- start | stop | restart
  status        TEXT NOT NULL,          -- pending | running | succeeded | failed | interrupted
  requested_at  TEXT NOT NULL,
  finished_at   TEXT,
  error         TEXT NOT NULL DEFAULT '',
  state_before  TEXT,                   -- NULL = it was not known either
  state_after   TEXT                    -- NULL = not read back
);

CREATE INDEX azure_actions_recent ON azure_actions(requested_at DESC);

-- One action in flight per resource, enforced by the database rather than by a
-- check-then-insert that two requests can interleave through.
CREATE UNIQUE INDEX azure_actions_in_flight
  ON azure_actions(resource_id) WHERE status IN ('pending','running');
```

No foreign key to `azure_resources`, and `resource_name` copied, for the reason `azure_costs` has neither: **a resource deleted next week must not erase the record that it was stopped today.**

- [ ] **Step 2: Write the failing tests**

- `TestStartWritesPendingBeforeAnythingElse` — after `StartAzureAction` the row is readable with `status = "pending"` and a `requested_at`, and `finished_at` is `NULL`.
- `TestASecondActionOnTheSameResourceIsRefused` — a second `StartAzureAction` returns `ErrActionInFlight`; **and** the same test asserts a different resource is *not* refused, so the partial index is not accidentally global.
- `TestAFinishedActionFreesTheResource` — after `FinishAzureAction`, a new action starts.
- `TestInterruptedOnStartup` — two rows (`pending`, `running`) plus one `succeeded`; `InterruptAzureActions` returns 2, marks exactly those two `interrupted`, leaves `state_after` `NULL`, and does not touch the third.
- `TestListIsMostRecentFirstAndLimited`.
- `TestSchemaVersionIsFour` — the two assertions in `store_test.go` move from 3 to 4, and `azure_actions` joins the expected table list. (Lot 2 and lot 3 each hit this; it is not a surprise, it is a checklist item.)

- [ ] **Step 3: Implement, then verify** — `go test ./internal/hub/store/...`.

---

### Task 4: The hub runs the action

**Files:**
- Create: `internal/hub/azure_action.go`, `internal/hub/azure_action_test.go`
- Modify: `internal/hub/hub.go` (interrupt on startup)

**Interfaces:**
```go
// StartAction validates, records, and runs the action off the caller's request.
// It returns the action id as soon as the row exists.
func (h *Hub) StartAction(ctx context.Context, resourceID string, a azure.Action) (int64, error)

var ErrAzureOff = errors.New("hub: Azure is not configured")
var ErrNotActionable = errors.New("hub: this resource type has no actions")
```

- [ ] **Step 1: Write the failing tests** (against an `httptest` ARM, as `azure_test.go` already does)

- `TestStartActionRefusesAnUnknownResource` — an id absent from `azure_resources` is refused, **and the fake ARM records zero calls**. The hub does not act on an id it cannot show.
- `TestStartActionRefusesOutsideTheConfiguredGroups` — a well-formed id in another resource group, present in no inventory, same outcome. The browser is not a security boundary.
- `TestStartActionRefusesWhenAzureIsOff` — `ErrAzureOff`, no row written.
- `TestSuccessfulActionReadsTheStateBack` — the fake answers 200 to `/stop`, then `{"properties":{"state":"Stopped"}}` to the enrichment read. The row ends `succeeded` with `state_after = "Stopped"`, **and `azure_resources.state` is updated to the value that was read**.
- `TestTheStateIsNeverInferred` — the fake answers 200 to `/stop` and then `{"properties":{"state":"Running"}}` to the read (Azure has not caught up yet). The stored state is **`Running`**, not `Stopped`. This test is the whole §5 of the spec: it fails the day someone writes the optimistic shortcut.
- `TestAFailedReadBackLeavesTheStateAlone` — 200 on the action, 500 on the read; the action is `succeeded`, `state_after` is `NULL`, and `azure_resources.state` keeps its previous value. Never a wrong certainty.
- `TestAFailedActionKeepsAzuresMessage` — a 403 with `AuthorizationFailed`; the row is `failed` and `error` contains Azure's own sentence.
- `TestBothOutcomesArePublished` — a subscriber to the bus sees an event for the success case **and** for the failure case. The defect found by hand in lot 3 was a failure nobody published; this is that lesson as a test.
- `TestInterruptedActionsAreNotReplayed` — a `running` row plus a fresh hub: after startup it is `interrupted` and **the fake ARM was never called**.

- [ ] **Step 2: Implement**

`StartAction`: look the resource up in the store (that is the scope check, the inventory *is* the allow-list); reject if the type is not in `azure.Actionable`; `StartAzureAction` with `state_before` taken from the row; spawn the worker; return the id.

The worker: `MarkAzureActionRunning`, `azure.Do`, then on success `azure.ReadState` and — only if the read succeeded — write the state onto `azure_resources`; `FinishAzureAction`; `h.bus.Publish("azure_action", …)` on every path, including failure. The goroutine takes a context tied to the hub's lifetime, not to the HTTP request, which dies the moment the 202 is written.

Startup: `InterruptAzureActions` runs where the lot 2 delivery replay runs, and logs the count.

- [ ] **Step 3: Verify** — `go test ./internal/hub/...`.

---

### Task 5: The API

**Files:**
- Modify: `internal/hub/server/api_azure.go`, `internal/hub/server/server.go` (two routes)
- Test: `internal/hub/server/api_azure_test.go`

```
POST /api/v1/azure/actions   {"resource_id":"…","action":"stop"}  → 202 {"action_id":17}
GET  /api/v1/azure/actions                                        → {"actions":[…]}
```

Both behind `s.auth`. **The resource id travels in the body, not the path**: an ARM id is mostly slashes, a Go 1.22 `ServeMux` wildcard does not match `/`, and percent-encoding it would make the route depend on when `net/http` unescapes the path — green in a test, broken behind a proxy.

- [ ] **Step 1: Write the failing tests** (the `rig` harness in `server_test.go`, as lot 3's tests already use — do not invent a harness)

- `TestActionRequiresAuth` — no cookie, 401, nothing written.
- `TestUnknownActionIsRejected` — `{"action":"delete"}` is a 400. The route does not forward a verb it has not vetted to ARM.
- `TestUnknownResourceIs404`, `TestInFlightIs409` — and the 409 body carries the running action's id, so the page can point at it.
- `TestAcceptedReturnsTheActionID` — 202 with `action_id`.
- `TestTheLogIsReadable` — `GET` returns the rows, most recent first, with their error text.
- `TestStatusMappingIsNotGuessedFromText` — a 403 from ARM surfaces as the "missing role" message; the assertion reads the typed status through the store's `error` column, never by matching English words.

- [ ] **Step 2: Implement, then verify** — `go test ./internal/hub/server/...`.

---

### Task 6: The page

**Files:**
- Modify: `web/src/routes/azure/+page.svelte`, `web/src/lib/azure.ts`
- Test: `web/src/lib/azure.test.ts`

- [ ] **Step 1: Pure helpers first, with tests**

In `azure.ts`: `actionsFor(resourceType)` (empty for a type that is not actionable — the page must not show three buttons that all 400), `isInFlight(resourceID, actions)`, `actionLabel(a)`. These are the parts that can be tested without a browser, so they are where the logic lives.

- [ ] **Step 2: The page**

- start / stop / restart per actionable resource, **disabled while one is in flight**, and disabled entirely when Azure is off or the last sync failed.
- **Stop and restart confirm, naming the resource.** `sandboxmgr`, `visualrami` and `urbanexplorer` are sites that are up. Start does not confirm: the worst case is a resource running that was not.
- A resource whose state is `NULL` still gets its buttons. Not knowing the state is not a reason to forbid acting.
- The twenty most recent actions are shown with their outcome and their error, `interrupted` included — the honest answer to "did my stop go through?" is sometimes "nobody knows, the hub was killed".
- The SSE `azure_action` event refreshes the list; no polling loop.

- [ ] **Step 3: Verify** — `npm run test`, `npx svelte-check` at zero errors, `make web` then `go build ./...`.

---

### Task 7: Proof, documentation and the branch

**Files:** `README.md`, `docs/ARCHITECTURE_EN.md`, `docs/ARCHITECTURE.md`, `TODOS.md`, `CHANGES.md`, `MEMORY.md`

- [ ] **Step 1: Run it by hand** against the fake-ARM hub: perform a stop, watch the row go `pending` → `running` → `succeeded`, watch the state change in the table, kill the hub mid-action and confirm the row reads `interrupted` after restart and that nothing was replayed. Check the browser console is clean.
- [ ] **Step 2: Documentation.** README: the actions exist, they need **Website Contributor**, and **no real action has ever run**. `ARCHITECTURE_EN.md` and its French mirror `ARCHITECTURE.md` **in the same commit** — the action lifecycle, and the two departures from lot 2 (no replay, 429-only retry).
- [ ] **Step 3: `TODOS.md`** — lot 4 done, and the "À prouver hors tests" line kept open: one real App Service must start or stop before this is called verified.
- [ ] **Step 4:** push the branch, open the PR, CI green on the three jobs.

---

## Verification checklist

- [ ] A 500 on an action is not retried (task 2), and an interrupted action is not replayed (task 4). Both are tests, not comments.
- [ ] No code path writes a state it did not read from Azure.
- [ ] The in-flight guard is a database constraint, not a check-then-insert.
- [ ] Both success and failure reach the bus.
- [ ] `go vet ./...` clean, `svelte-check` zero, CI green.
- [ ] The README does not claim Azure actions are verified.
