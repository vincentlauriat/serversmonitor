# ServersMonitor Lot 5 (VM provisioning) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** From the Azure page, create a Linux VM in the sandbox with the agent already installed and reporting, and delete it again when it is no longer wanted. Every resource created is recorded as it lands; nothing is ever deleted without a person asking for it by name.

**Architecture:** `internal/hub/azure` gains an async call path (202 + `Azure-AsyncOperation` polling), a provisioning module that issues the NIC and VM `PUT`s, and a delete module. The hub owns the lifecycle exactly as it does for actions: it writes the record before the first call, runs the run off the request, and publishes each step. `azure_provisions` + `azure_provision_resources` are the audit log the page reads.

**Tech Stack:** Go standard library only, as in lots 1-4. No Azure SDK.

**Spec:** `docs/superpowers/specs/2026-09-21-lot5-vm-provisioning-design.md`

## Global Constraints

- Branch `feat/lot5-vm-provisioning`; never commit to `main`. Conventional commits, English.
- **No `Co-Authored-By: Claude …` trailer on any commit** (`~/DevApps/CLAUDE.md`).
- **The hub never creates a network.** No VNet, no public IP, no NSG — the role forbids all three (spec §1). The subnet is configured and joined, or provisioning is refused.
- **Partial creation is recorded, never rolled back** (spec §4). No automatic delete, anywhere, ever — including in the failure path.
- **Deleting is user-initiated, name-confirmed, and tag-restricted** to `createdBy=ServersMonitor` (spec §6).
- **The host token never leaves the hub twice.** It goes into `customData` at creation and is never logged, never returned by the API, never stored in a provision row (spec §5).
- **A PUT create retries 5xx; a POST action does not.** A `PUT` on a named resource id is idempotent by construction — repeating it converges on the same resource. `POST …/deallocate` is not, which is why `retryThrottlingOnly` exists. Two policies, deliberately.
- Every task boundary leaves `go build ./...` green, `go vet ./...` clean, `svelte-check` at zero errors.
- No test may reach the network. Every Azure endpoint is an `httptest` server.

---

## API versions, read back from the tenant on 2026-09-21

Not recalled — `az provider show --namespace … --query "resourceTypes[?resourceType=='…'].apiVersions"`:

| Provider | Version used | Why |
|---|---|---|
| `Microsoft.Compute/virtualMachines` | `2024-11-01` | GA, present in the tenant, and `2023-12-01` (the Web version) is **not** a Compute version at all |
| `Microsoft.Network/networkInterfaces` | `2024-10-01` | GA, present in the tenant |
| `Microsoft.Web/sites` | `2023-12-01` | unchanged from lot 3/4 |

This is the whole reason task 2 exists: `webAPIVersion` is one package const shared by `Do` and `ReadState`, and a VM call sent with the Web version is a 400 that no fake would catch.

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/hub/azure/client.go` | *(modify)* `Response{Status, Header, Body}`, `PutAsync`/`PostAsync`, `retryIdempotent` |
| `internal/hub/azure/async.go` | *(new)* `Azure-AsyncOperation` / `Location` polling |
| `internal/hub/azure/actions.go` | *(modify)* per-type api-version and state reader; VMs added to `Actionable` |
| `internal/hub/azure/provision.go` | *(new)* `CreateVM` — NIC then VM, tagged |
| `internal/hub/azure/delete.go` | *(new)* `DeleteVM` — VM, disk, NIC, tag-checked |
| `internal/hub/azure/cloudinit.go` | *(new)* the cloud-init document |
| `internal/hub/azure/provision_config.go` | *(new)* `ProvisionConfig`, loaded from settings, validated at provision time |
| `internal/hub/store/migrations/0005_provisions.sql` | *(new)* `azure_provisions`, `azure_provision_resources` |
| `internal/hub/store/azure_provisions.go` | *(new)* start / record / finish / interrupt / list |
| `internal/hub/azure_provision.go` | *(new)* the hub's provisioning lifecycle |
| `internal/hub/server/api_azure.go` | *(modify)* the three `/api/v1/azure/vms` routes |
| `internal/hub/server/server.go` | *(modify)* route registration |
| `web/src/lib/azure.ts` | *(modify)* provision helpers, unit-tested |
| `web/src/routes/azure/+page.svelte` | *(modify)* New VM form, provisions list, per-resource delete |
| `docs/ARCHITECTURE_EN.md` + `docs/ARCHITECTURE.md` | *(modify)* the async call path and the provisioning tables, both in the same commit |

---

### Task 1: The client learns to read a response, not just a body

**Files:**
- Modify: `internal/hub/azure/client.go`
- Test: `internal/hub/azure/client_test.go`

**Why first.** `attempt` returns `([]byte, time.Duration, error)`. The status and every response header are discarded before `doWith` sees them. A 202 is indistinguishable from a 200, and `Azure-AsyncOperation` never reaches any caller. Lot 4's spec said adding VMs would be "a row, not a redesign"; that was measured against a client that cannot do this.

**The rippling problem, and how it is contained.** `attempt` feeds `doWith` → `do` → `GetAll`/`Get`/`Post`/`PostAction`, all of which lots 3 and 4 call and test. Changing their signatures would rewrite call sites that have nothing to do with this lot. So:

```go
// Response is what an ARM call answered, for the callers that need more than
// the body. 202 versus 200 is the whole difference between "done" and "started".
type Response struct {
    Status int
    Header http.Header
    Body   []byte
}

func (c *Client) doResponse(ctx context.Context, method, rawURL string, body []byte,
    retry func(error) bool) (Response, error)

// do keeps its signature; every lot 3 and lot 4 call site is untouched.
func (c *Client) do(ctx context.Context, method, rawURL string, body []byte) ([]byte, error) {
    r, err := c.doResponse(ctx, method, rawURL, body, Retryable)
    return r.Body, err
}
```

**And the second retry policy, with its reason in the code:**

```go
// retryIdempotent retries anything Retryable, 5xx included. A PUT on a named
// resource id converges: repeating it produces the same resource, not a second
// one. This is exactly why retryThrottlingOnly is narrower — a POST …/deallocate
// repeated after a 5xx may act twice.
func retryIdempotent(err error) bool { return Retryable(err) }
```

**Step 1: Write the tests**
- A handler answering 202 with `Azure-AsyncOperation: <url>`; assert `Response.Status == 202` and the header survives.
- A handler answering 200; assert `Status == 200` and `Body` matches.
- **The containment test:** an existing `GetAll` paging test and an existing `PostAction` 429 test still pass unchanged. Run the lot 3 and lot 4 suites, not a copy.
- `retryIdempotent` on a 503 retries; `retryThrottlingOnly` on the same 503 does not. Both against one fake counting calls.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/azure/...`. **Mutation check:** make `doResponse` drop the header map; the 202 test must fail on the missing `Azure-AsyncOperation`, not on a nil map panic.

---

### Task 2: api-version and state are properties of the resource type, not of the package

**Files:**
- Modify: `internal/hub/azure/actions.go`
- Test: `internal/hub/azure/actions_test.go`

**Why.** Two package-level assumptions are Web-shaped and silently wrong for Compute:

1. `const webAPIVersion = "2023-12-01"` is used by both `Do` and `ReadState`. It is **not a Compute api-version** (verified above). A VM start sent with it is a 400.
2. `ReadState` reads `properties.state`. A VM has no such field: its power state is in `/instanceView` → `statuses[]` → `code == "PowerState/running"`. And `ReadState` returns `(nil, nil)` when the field is absent — so against a VM it would report **"nobody knows the state"** rather than failing. That is the silent-empty this project refuses everywhere else, and it would be indistinguishable from a genuine unread state.

**Interfaces:**
```go
// provider describes one resource type: how to address it, and how to read a
// state out of what it answers. Keeping both on the type is what stops a Web
// api-version reaching a Compute path.
type provider struct {
    apiVersion string
    verbs      map[Action]string
    // stateURL returns the path and query the state read uses; a VM needs
    // /instanceView, a site reads itself.
    stateURL func(armID string) (string, url.Values)
    readState func(body []byte) (*string, error)
}

var providers = map[string]provider{
    "microsoft.web/sites":              {…, verbs: {start, stop, restart}},
    "microsoft.compute/virtualmachines": {…, verbs: {start: "start", stop: "deallocate", restart: "restart"}},
}
```

`Supports`, `Do` and `ReadState` keep their signatures. `Actionable` becomes derived (or is removed if nothing outside the package reads it — check before deleting).

**Step 1: Write the tests**
- `Do(…, "Microsoft.Compute/virtualMachines", ActionStop)` hits `…/deallocate?api-version=2024-11-01`. Assert the **whole URL**, api-version included.
- `Do(…, "Microsoft.Web/sites", ActionStop)` still hits `…/stop?api-version=2023-12-01` — unchanged.
- VM `ReadState` against an `instanceView` body containing `PowerState/running` → `"running"`; against one with only `ProvisioningState/succeeded` → `(nil, nil)`, because a provisioning state is not a power state.
- **The counter-assertion:** a VM `instanceView` body fed to the *Web* reader must not yield a state. Documents the trap rather than leaving it for lot 6.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/azure/...`. **Mutation check:** hard-code the Web api-version back into the VM provider; the URL assertion must fail.

---

### Task 3: The 202 poll

**Files:**
- Create: `internal/hub/azure/async.go`
- Test: `internal/hub/azure/async_test.go`

**Why.** Every Compute call — start, deallocate, restart, the VM `PUT`, every delete — answers 202 and finishes later. Without the poll the hub would report success the moment Azure said "I heard you".

**The two headers are not interchangeable.** `Azure-AsyncOperation` returns a JSON body with a `status` field (`InProgress` / `Succeeded` / `Failed` / `Canceled`) and is authoritative. `Location` returns the *result* and signals completion by answering something other than 202 — there is no `status` field to read. Preferring the first and falling back to the second means two code paths, not one URL swap.

**Interfaces:**
```go
// Await follows a 202 to its terminal state. It returns nil when the operation
// succeeded, an *Error when Azure reported a failure, and ctx.Err() when the
// caller's budget ran out — which is not a failure of the operation, and the
// caller must not record it as one.
func Await(ctx context.Context, c *Client, r Response) error
```

- `Retry-After` on each poll response is honoured; absent, the backoff ladder applies with a floor of 1 s and a ceiling of 30 s.
- A poll chain is bounded the way `GetAll` is (`maxPolls`), so a server that never terminates cannot hang a provision forever.
- A 202 with **neither** header is an error naming that fact, not a silent success.

**Step 1: Write the tests**
- `Azure-AsyncOperation` returning `InProgress` twice then `Succeeded` → nil, three polls.
- `Failed` with an ARM error body → an `*Error` carrying ARM's `code`, asserted by `errors.As`.
- `Retry-After: 2` is honoured: a fake `Sleep` records the durations.
- `Location` fallback: 202, 202, then 200 → nil.
- A 202 with no header at all → an error mentioning both header names.
- Context cancelled mid-poll → `ctx.Err()`, **and the test asserts the returned error is not an `*Error`** — a shutdown must never be recorded as an Azure failure.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/azure/...`. **Mutation check:** treat a 202 answer to the poll as terminal success; the two-`InProgress` test must fail.

---

### Task 4: VMs become actionable

**Files:**
- Modify: `internal/hub/azure/actions.go`, `internal/hub/azure_action.go`
- Test: `internal/hub/azure/actions_test.go`, `internal/hub/azure_test.go`

**Why here.** Tasks 1-3 exist for this. It is also the smallest possible first use of the poll, against a code path lot 4 already proved end to end — a better place to find a poll bug than inside a six-call provision.

`Do` awaits its own 202. `runAction` in `azure_action.go` is unchanged in shape, and **`running` finally means something**: a VM stop stays `running` for the length of the poll instead of for the length of one HTTP call.

**Step 1: Write the tests**
- A hub-level test: stop a VM in the inventory; the fake answers 202 then `InProgress` then `Succeeded`; the row goes `pending` → `running` → `succeeded`, and `state_after` comes from the instance-view read-back.
- The read-back budget (`h.readBack`) already bounds the state read; assert a failing read-back still leaves the action `succeeded` with `state_after` NULL — the lot 4 guarantee, now over a slower path.
- **Interruption matters more now:** a `running` VM action + hub restart → `interrupted`, nothing replayed.

**Step 2: Implement**

**Step 3: Verify** — `go test ./...`. **Mutation check:** make `Do` return before awaiting; the row must reach `succeeded` before the fake reports `Succeeded`, and the state assertion must fail.

---

### Task 5: Provisioning settings — refused at provision time, not at save time

**Files:**
- Create: `internal/hub/azure/provision_config.go`
- Test: `internal/hub/azure/provision_config_test.go`

**Interfaces:**
```go
type ProvisionConfig struct {
    SubnetID   string // the full ARM id of an existing subnet
    HubURL     string // what the agent dials; the hub does not guess its own address
    Size       string // default "Standard_B1s"
    Image      string // default "Canonical:ubuntu-24_04-lts:server:latest"
    AdminUser  string // default "azureuser"
    SSHKey     string // authorized_keys line
}

// Ready reports what is missing, by name. Provisioning is refused; saving an
// Azure configuration is not.
func (p ProvisionConfig) Ready() error
```

**Why not in `Config.Validate`.** `Validate` runs when the Azure settings are saved. Putting the subnet and the hub URL there would stop someone who only wants lot 3's read-only inventory from saving anything at all. The spec says provisioning is refused when they are empty — that is a different moment, and a different message.

**Step 1: Write the tests**
- Empty subnet → `Ready` names the subnet; empty hub URL → names the hub URL; both empty → names both in one message.
- A `HubURL` of `http://localhost:8091` is rejected with the reason spelled out: a VM in Azure cannot resolve the hub's loopback. (`localhost`, `127.0.0.0/8`, `::1`.) This is the failure mode the spec calls out in §1 and it is cheap to catch here instead of in twenty minutes of cloud-init.
- Defaults apply when the keys are absent; a saved value overrides them.

**Step 2: Implement** — `LoadProvisionConfig` / `SaveProvisionConfig` alongside the existing pair, same settings table.

**Step 3: Verify** — `go test ./internal/hub/azure/...`.

---

### Task 6: The provision record

**Files:**
- Create: `internal/hub/store/migrations/0005_provisions.sql`, `internal/hub/store/azure_provisions.go`
- Test: `internal/hub/store/azure_provisions_test.go`

**Why two tables.** One provision creates several resources at several moments, and §4 requires naming each leftover individually. A single row with a JSON blob could not carry a per-resource delete button honestly.

```sql
CREATE TABLE azure_provisions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,          -- the VM name the user asked for
  host_id       INTEGER,                -- the host row created first (§5); no FK: the host may be deleted
  status        TEXT NOT NULL,          -- pending | running | succeeded | failed | interrupted
  requested_at  TEXT NOT NULL,
  finished_at   TEXT,
  error         TEXT
);
CREATE TABLE azure_provision_resources (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  provision_id  INTEGER NOT NULL REFERENCES azure_provisions(id) ON DELETE CASCADE,
  arm_id        TEXT NOT NULL,          -- ARM's own casing: the delete URL is built from it
  kind          TEXT NOT NULL,          -- nic | vm | disk
  created_at    TEXT NOT NULL,
  deleted_at    TEXT                    -- set when the user deletes it, never by the hub alone
);
CREATE UNIQUE INDEX azure_provisions_in_flight ON azure_provisions(name)
  WHERE status IN ('pending','running');
```

The partial unique index is lot 4's "one in flight per resource", applied to the name: two provisions of the same VM name racing would both `PUT` the same id. Detection is the same `*sqlite.Error.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE` path — reuse it, do not re-derive it.

**Step 1: Write the tests**
- Start, record two resources, finish succeeded; `ListAzureProvisions` returns them nested and newest-first.
- A second start with a name already in flight → `ErrProvisionInFlight`, asserted by `errors.Is`, **and the test asserts the error came from the constraint** rather than from a pre-flight `SELECT` (which would race).
- `InterruptAzureProvisions` turns `pending`/`running` into `interrupted` and leaves the recorded resources intact — that is the whole point: **an interrupted provision may have created things**.
- `MarkProvisionResourceDeleted` sets `deleted_at`; a second call is a no-op, not an error.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/store/...`. **Mutation check:** drop the `WHERE status IN …` clause from the index; the "a finished provision does not block a new one with the same name" test must fail.

---

### Task 7: Cloud-init, and the token that rides in it

**Files:**
- Create: `internal/hub/azure/cloudinit.go`
- Test: `internal/hub/azure/cloudinit_test.go`

**Interfaces:**
```go
// CloudInit returns the base64 customData that installs the agent and points it
// at the hub. The token is a parameter and never a field: nothing holds it after
// this returns.
func CloudInit(hubURL, token string) (string, error)
```

**Step 1: Write the tests**
- The decoded document starts with `#cloud-config` and contains the hub URL and the token.
- **The token never appears anywhere else:** a test renders a provision through the whole hub path with a token of a known sentinel value, then asserts the sentinel is absent from every API response body, every provision row, and the captured log output. This is the §5 promise, and it is the one test that would actually catch a regression.
- A `hubURL` with a trailing slash produces the same document as one without.

**Step 2: Implement** — write the token to a file with mode `0600` via `write_files`, not into a systemd unit's `Environment=` where `systemctl show` would print it.

**Step 3: Verify** — `go test ./internal/hub/azure/...`. **Mutation check:** log the rendered document at debug level; the sentinel test must fail.

---

### Task 8: `CreateVM` — two PUTs, both tagged

**Files:**
- Create: `internal/hub/azure/provision.go`
- Test: `internal/hub/azure/provision_test.go`

**Interfaces:**
```go
// Created is one resource that now exists. It is returned as each PUT lands, so
// a failure halfway still tells the caller what was created.
type Created struct {
    ARMID string
    Kind  string // nic | vm | disk
}

// CreateVM issues the NIC PUT then the VM PUT, awaiting each 202. onCreated is
// called as each resource lands — before the next call is attempted, so a
// crash between the two still leaves the NIC recorded.
func CreateVM(ctx context.Context, c *Client, p ProvisionConfig, req CreateRequest,
    onCreated func(Created) error) error
```

Every resource carries `createdBy=ServersMonitor`; the VM also carries `smHostId=<host row id>`. The OS disk is created implicitly by the VM `PUT` — its id is read back from the VM's `properties.storageProfile.osDisk.managedDisk.id` **after** the PUT succeeds and recorded as a third `Created`, because task 10 has to delete it and guessing its name would be a bet.

**Step 1: Write the tests**
- Call order asserted by a fake recording paths: NIC before VM, each `PUT`, each awaited.
- Both bodies carry the tag; the VM body carries `smHostId`.
- The NIC body has **no** `publicIPAddress` — asserted explicitly, because the role forbids it and a template copied from a tutorial would include one.
- The NIC is joined to the configured subnet id, verbatim.
- **Mid-run failure:** the NIC succeeds and the VM `PUT` answers 403. `onCreated` was called once, with the NIC, and `CreateVM` returns the 403. Assert **no DELETE was ever sent** — this is §4 as an executable statement, and it is the assertion that stops a future "helpful" rollback.
- `onCreated` returning an error aborts before the next call: if the hub cannot record what it created, it must not create more.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/azure/...`. **Mutation check:** add a cleanup delete on the VM failure path; the "no DELETE" assertion must fail.

---

### Task 9: The hub's provisioning lifecycle

**Files:**
- Create: `internal/hub/azure_provision.go`
- Modify: `internal/hub/hub.go` (call `interruptProvisions` from `Run`, beside `interruptActions`)
- Test: `internal/hub/azure_provision_test.go`

**Order, and why.** The host row is created **first** (`CreateHost` returns the token), so a VM that boots and dials in finds itself expected. Then the provision row, then the calls. A provision that fails leaves a host row that never connected, and the page shows it as such rather than deleting it.

```go
func (h *Hub) StartProvision(name string) (int64, error)  // 202-shaped: returns as soon as the row exists
func (h *Hub) runProvision(provisionID int64, …)          // off the request, on h.baseCtx()
```

**Step 1: Write the tests**
- Happy path against a fake ARM: host row created, provision `pending` → `running` → `succeeded`, three resources recorded, `azure_provision` published on the bus.
- Azure off → `ErrAzureOff`. Subnet missing → the `Ready` error, and **no host row was created** — a refused provision must not leave a phantom host.
- Mid-run failure: provision `failed`, the NIC recorded, the host row still there, the error text stored, **and the event published** (lot 3's defect was a failure nobody published; do not reintroduce it).
- A `running` provision + hub restart → `interrupted` with its resources intact.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/...`.

---

### Task 10: Delete — user-initiated, name-confirmed, tag-restricted

**Files:**
- Create: `internal/hub/azure/delete.go`; modify `internal/hub/azure_provision.go`
- Test: `internal/hub/azure/delete_test.go`, `internal/hub/azure_provision_test.go`

**Order:** VM, then OS disk, then NIC. Azure refuses to delete a disk or a NIC still attached, so this order is not a preference.

**Before each DELETE the hub reads the resource and checks its tags.** A resource without `createdBy=ServersMonitor` is refused with an error naming it. This is the guard that makes a mis-typed id harmless.

**Step 1: Write the tests**
- Order asserted: VM, disk, NIC.
- An untagged VM → refused, **and no DELETE was sent at all**, not even for the NIC.
- A tag read that fails (5xx through the ladder) → refused. Not knowing is not permission.
- A 404 on the VM is **not** an error: something already deleted is deleted. The disk and NIC are still attempted.
- Hub level: a wrong `confirm_name` → refused before any call; the right one proceeds.
- Each deleted resource gets `deleted_at`; the host row is untouched.

**Step 2: Implement**

**Step 3: Verify** — `go test ./...`. **Mutation check:** skip the tag check; the untagged-VM test must fail on a DELETE that was sent.

---

### Task 11: The three routes

**Files:**
- Modify: `internal/hub/server/api_azure.go`, `internal/hub/server/server.go`
- Test: `internal/hub/server/api_azure_test.go`

```
POST /api/v1/azure/vms          {"name":"…","size":"…"}                      → 202 {"provision_id": 4}
GET  /api/v1/azure/vms                                                        → provisions + resources
POST /api/v1/azure/vms/delete   {"resource_id":"…","confirm_name":"…"}        → 202
```

Ids travel in bodies, not paths — Go 1.22 `ServeMux` wildcards do not match `/`, and an ARM id is all slashes. This is lot 4's decision, unchanged.

Status mapping, extending `Azurer`: `ErrProvisionInFlight` → 409, `ErrNotConfigured` / a `Ready` failure → 400 with the missing field named, a refused delete → 400, anything else → 500. **No message ever contains the token**; a test asserts it.

**Step 1: Write the tests** — one per status, plus: a `GET` with no provisions returns `[]`, never `null`; the name is validated against Azure's VM naming rules before anything else (1-64 chars, no trailing dash) with the rule named in the message.

**Step 2: Implement**

**Step 3: Verify** — `go test ./internal/hub/server/...`.

---

### Task 12: The page

**Files:**
- Modify: `web/src/lib/azure.ts`, `web/src/routes/azure/+page.svelte`
- Test: `web/src/lib/azure.test.ts`

- A **New VM** form: name, size. Disabled with the reason shown when provisioning is not configured — the message names the missing setting, it does not just grey out.
- A **provisions** list: each run, its status, its resources, its error. A failed or interrupted run shows its leftovers **named**, each with its own Delete button.
- Delete asks for the VM name to be typed, not a `confirm()`. Reuse nothing from the lot 4 confirmation: that one is dismissible by reflex, and that is the difference §6 insists on.
- Subscribe to `azure_provision` on the SSE bus. `live.svelte.ts` already attaches any type registered through `on()` since lot 4's fix — verify that, do not assume it.

**Step 1: Write the tests** — pure helpers in `azure.ts`: `provisionOutcome`, `leftovers(provision)`, `canProvision(config)`, `deleteConfirmed(typed, name)`.

**Step 2: Implement**

**Step 3: Verify** — `npm run check`, `npm run build`, then `make build` so the embedded `webdist` is rebuilt.

---

### Task 13: Documentation, in the same commit as the code it describes

**Files:** `docs/ARCHITECTURE_EN.md` + `docs/ARCHITECTURE.md` (strict mirror), `README.md`, `TODOS.md`, `CHANGES.md`, `MEMORY.md`

- The async call path and the two provisioning tables go in the architecture diagram and tables — both language versions, same commit.
- `README.md`: the Features table and the roadmap.
- **`TODOS.md` → "À prouver hors tests"** — written before task 1, not after task 13 (spec §8): *that a VM was ever created, that cloud-init ran, that an agent installed this way ever dialled in.*

---

## What this plan does not do

- **No VNet, no public IP, no NSG.** Out of the role's reach (spec §1), and the plan does not work around it.
- **No rollback.** Not a simplification — a decision (spec §4).
- **No VM resize, no reimage, no snapshot.** Create, act, delete.
- **No proof that any of it works against Azure.** Third lot running. The admin ask, unchanged and now covering lots 3, 4 and 5 together: **Reader + Website Contributor + Virtual Machine Contributor** on `rg-dev-vincent-sandbox`, plus a VNet with a subnet that Vincent creates himself.
