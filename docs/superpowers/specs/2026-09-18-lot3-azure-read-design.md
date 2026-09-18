# Lot 3 — Azure read: inventory, state and cost

**Goal.** ServersMonitor shows, without being asked, every resource in the configured Azure scope:
what it is, whether it is running, and what it has cost this month.

**Read only.** Start, stop and restart are lot 4. VM provisioning is lot 5. Acting on cost is lot 6.
Nothing in this lot writes to Azure.

---

## 1. The precondition this lot cannot remove

Verified against the live tenant on 2026-09-18, not assumed:

- `policies/authorizationPolicy` returns `allowedToCreateApps: false`. No app registration, therefore
  no service principal.
- Effective permissions on the target resource group are `actions: ['*']` with `notActions`
  containing `Microsoft.Authorization/*/Write`. A user-assigned managed identity can be *created*
  but cannot be *granted* anything, so it would read nothing.

**No daemon credential exists in this tenant without a tenant administrator acting once**, either:

- an app registration with a client secret, plus **Reader** on the resource group; or
- **Reader** on the resource group, granted to a managed identity.

**Reader alone is enough, cost included.** This is worth stating because it looks wrong. The cost
query is an HTTP `POST`, so the natural reading is that it needs an `/action` permission that
`Reader` (`*/read`) would not grant. Checked against the provider on 2026-09-18: the operation
behind that POST is `Microsoft.CostManagement/query/read`, displayed as "Query usage data". It is a
read. `Reader` covers it, and no second role is needed. Anyone tempted to add **Cost Management
Reader** to the ask should know it would not have helped either — its actions are also all `/read`,
so the two roles stand or fall together on the same operation name.

This is a dependency on another person with a lead time nobody here controls. Lot 3 therefore ships
fully tested and unable to read anything until that happens, and says so in the interface rather
than looking broken.

**The Azure CLI token is not a supported credential.** `az account get-access-token` works on a
developer's Mac and breaks two decisions already made: the deliverable is a portable Docker image
with no `az` binary, and the hub runs unattended. A one-hour token refreshed out of the CLI's MSAL
cache is not a daemon credential, and borrowing the CLI's first-party client id for a device-code
flow inside the hub is worse. If a local development shim is ever added it is labelled as such and
is never the default.

---

## 2. The invariant, restated for Azure

The lot 1 rule holds unchanged: **silence is never zero.**

| Situation | Stored | Shown |
|---|---|---|
| A resource whose state could not be read | `NULL` | a dash, not "Stopped" |
| A cost Azure has not reported yet | no row | a dash, not `0,00 €` |
| A sync that failed entirely | nothing written | the last good inventory, plus the failure and its time |

The third line is the one that matters most. **A failed sync must never empty the inventory.** An
expired secret, a revoked role or a network blip would otherwise turn "I cannot see Azure" into
"the sandbox is empty", which is the same class of lie as an agent's death reading as 0 % CPU.

---

## 3. Cost and inventory are separate facts

Measured on 2026-09-18: a month-to-date cost query on the sandbox returned **ten rows against six
live resources**. The extra four were a Log Analytics workspace and three resources that no longer
exist. A resource deleted mid-month still cost money for the days it ran.

So:

- Inventory rows and cost rows are stored separately, keyed by the ARM resource id, lowercased.
- They are joined **for display only**.
- A cost row with no live inventory row is shown as a **deleted resource**, never dropped. Dropping
  it would understate the bill, which is the one number a cost feature exists to get right.
- Inventory uses a soft delete: a resource that stops appearing gets `deleted_at` set and keeps its
  name, so a cost row can still be labelled. A resource deleted before ServersMonitor ever ran has
  no name, and the last segment of its resource id is shown instead.

---

## 4. Inventory needs two passes

Also measured: the generic ARM resource list returns `properties: null`. It carries the name, type,
location and tags, and no state and no SKU. The typed provider list returns `state`, `kind`,
`httpsOnly` and `defaultHostName`.

"Remonter toutes les ressources" means state, not just names, so:

1. **Catalogue pass** — `GET /subscriptions/{sub}/resourceGroups/{rg}/resources?api-version=2021-04-01`.
   Works for every resource type, known or not. This is the list of what exists.
2. **Enrichment pass** — one typed call per provider we understand. Lot 3 understands
   `Microsoft.Web`: `GET .../providers/Microsoft.Web/sites?api-version=2023-12-01` and the matching
   `serverfarms` call, which add `state`, `kind` and the default host name.

A resource of a type with no enrichment is still listed, with a null state. **An unknown type is
shown, not hidden**: an inventory that silently omits what it does not understand is worse than one
that admits the gap.

---

## 5. Authentication

One interface, two implementations that will actually run in production.

```go
// Token is an access token and the instant it stops being usable.
type Token struct { Value string; ExpiresAt time.Time }

// Source produces a bearer token for Azure Resource Manager.
type Source interface {
    Name() string                                  // "client_secret" | "managed_identity"
    Token(ctx context.Context) (Token, error)
}
```

A caching wrapper holds the token and refreshes it **five minutes before expiry**, so a request
never races the boundary.

### 5.1 Client secret

Verified against Microsoft Learn on 2026-09-18:

```
POST https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials
&client_id={client id}
&client_secret={secret, URL-encoded}
&scope=https%3A%2F%2Fmanagement.azure.com%2F.default
```

Success is `{"token_type":"Bearer","expires_in":3599,"access_token":"…"}` — `expires_in` is a
**number**. Failure is `400` with `{"error":"…","error_description":"AADSTS…","error_codes":[…]}`,
and the `error_description` is what reaches the interface, because `AADSTS7000215` tells Vincent his
secret is wrong and "authentication failed" tells him nothing.

### 5.2 Managed identity

Two endpoints, chosen by what the environment exposes:

- **App Service / Container Apps**, when `IDENTITY_ENDPOINT` and `IDENTITY_HEADER` are both set:
  `GET {IDENTITY_ENDPOINT}?resource=https://management.azure.com/&api-version=2019-08-01`
  with header `X-IDENTITY-HEADER: {IDENTITY_HEADER}`.
- **Virtual machine (IMDS)** otherwise:
  `GET http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://management.azure.com/`
  with header `Metadata: true`.

A user-assigned identity adds `client_id={its client id}`. Omitting it asks for the system-assigned
one.

**The response shape differs from the client-secret one and between endpoints.** IMDS returns
`expires_in` and `expires_on` as **strings**; App Service returns `expires_on` as a string and no
`expires_in` at all. Parsing must accept a JSON number or a JSON string for `expires_in`, and fall
back to `expires_on` as a Unix epoch when `expires_in` is absent. This is asserted in tests against
all three recorded shapes, because getting it wrong yields a token that is either never refreshed or
refreshed on every call.

IMDS retry guidance, quoted from the same page: retry on `404`, `410`, `429` and `5xx`; a `410`
means the service is updating and will return within 70 seconds. Other `4xx` are design-time errors
and are not retried.

---

## 6. Calling ARM

One small client over `net/http`, in the taste already set by `docker.go` and `net/smtp`: no Azure
SDK. The surface used here is four GET shapes and one POST, all stable and versioned.

It must handle three things the SDK would have handled:

- **Pagination.** ARM returns `{"value":[…],"nextLink":"…"}`. Follow `nextLink` until absent. A
  client that reads only the first page silently truncates the inventory, which is the same failure
  as a sync that empties it, only quieter.
- **Throttling.** `429` with a `Retry-After` header. Honour the header when present, otherwise back
  off. A `429` was hit during reconnaissance on the cost endpoint, so this is not hypothetical.
- **Classification**, reusing lot 2's vocabulary: `429` and `5xx` are worth retrying, other `4xx`
  are not. A `403` means the role assignment is missing and will fail identically forever; it is
  surfaced verbatim, because it is the single most likely failure on first setup.

### Cost

```
POST /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.CostManagement/query?api-version=2023-11-01
{"type":"ActualCost","timeframe":"MonthToDate",
 "dataset":{"granularity":"None",
            "aggregation":{"total":{"name":"Cost","function":"Sum"}},
            "grouping":[{"type":"Dimension","name":"ResourceId"}]}}
```

The response is columnar: `properties.columns` names the columns and `properties.rows` holds the
values. **Column order is read from `columns`, never assumed by position.** Verified live: the
columns came back as `Cost`, `ResourceId`, `Currency`, which is not the order the request lists them
in.

Azure's cost data lags by hours. Each cost row therefore carries the instant it was fetched, and the
interface says when, so a low number reads as "not all reported yet" rather than "cheap".

---

## 7. Storage

Migration `0003_azure.sql`.

```sql
CREATE TABLE azure_resources (
  id                TEXT PRIMARY KEY,     -- ARM resource id, lowercased
  name              TEXT NOT NULL,
  type              TEXT NOT NULL,
  resource_group    TEXT NOT NULL,
  location          TEXT NOT NULL,
  kind              TEXT NOT NULL DEFAULT '',
  sku               TEXT NOT NULL DEFAULT '',
  state             TEXT,                 -- NULL = not read, never "unknown"
  provisioning_state TEXT NOT NULL DEFAULT '',
  host              TEXT NOT NULL DEFAULT '',
  tags              TEXT NOT NULL DEFAULT '{}',
  first_seen        TEXT NOT NULL,
  last_seen         TEXT NOT NULL,
  deleted_at        TEXT                  -- NULL = still there
);

CREATE TABLE azure_costs (
  resource_id TEXT NOT NULL,
  period      TEXT NOT NULL,              -- "2026-09"
  amount      REAL NOT NULL,
  currency    TEXT NOT NULL,
  as_of       TEXT NOT NULL,
  PRIMARY KEY (resource_id, period)
) WITHOUT ROWID;

CREATE TABLE azure_sync (
  scope      TEXT PRIMARY KEY,            -- "inventory" | "cost"
  ok         INTEGER NOT NULL,
  message    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at   TEXT NOT NULL
);
```

`azure_costs` has **no foreign key to `azure_resources`**, deliberately. A cost row for a resource
deleted before ServersMonitor first ran has no parent and must still be stored.

The inventory upsert is: mark every row in scope, upsert what the sync returned, then set
`deleted_at` on the rows in scope that were not seen. **Only on a successful sync.** A failed sync
writes nothing but the `azure_sync` row.

**A sync is successful only when every page of every call in it succeeded.** This has to be said
because §6 introduces pagination: if page three of five exhausts its retries and the sync is still
treated as a success, every resource listed on pages four and five is marked deleted, because the
page naming them never arrived. That is the emptied inventory again, only quieter and harder to
notice. Any failure anywhere in the sweep — a token refusal, a page that never came, a retry budget
spent — aborts the whole sync and writes nothing but the `azure_sync` row.

---

## 8. Configuration

In `settings`, alongside the notification settings, editable from the browser, same password rule.

| Key | Meaning |
|---|---|
| `azure_mode` | `off` (default), `client_secret`, `managed_identity` |
| `azure_tenant_id`, `azure_client_id`, `azure_client_secret` | client-secret mode |
| `azure_mi_client_id` | optional, selects a user-assigned identity |
| `azure_subscription_id` | the subscription |
| `azure_resource_groups` | comma-separated; **required**, at least one |
| `azure_inventory_interval_min` | default 15 |
| `azure_cost_interval_min` | default 60 |
| `azure_budget_monthly` | a number, `0` means none; it inherits the currency of the cost rows |

**A resource group is required, and subscription-wide scope is out of scope for this lot.** The
role assignment being asked for in §1 is on a resource group. A catalogue pass at
`/subscriptions/{sub}/resources`, or a cost query at subscription scope, needs a subscription-scope
assignment nobody is requesting, and would answer `403`. Saving an empty resource-group list is
rejected at save time with that reason, rather than producing a page of permission errors at the
next sync.

The client secret follows the SMTP rule exactly: reads expose `azure_client_secret_set: true|false`
and never the value; on write an absent field means leave it alone and an empty string clears it.

A **Test connection** button acquires a token and runs one catalogue call, returning Azure's own
error on failure. That button is how Vincent will find out, in one click, whether the administrator
granted the right role.

---

## 9. Interface

A new top-level page, **Azure**.

- A header: the scope, the last successful sync and, when the last attempt failed, a red line with
  Azure's own message and the time. Never an empty table without an explanation.
- A table: name, type, location, state, month-to-date cost, tags. Deleted resources are listed last,
  struck through, with their cost.
- A total against the monthly budget when one is set: spent, budget, and the share used. No alert,
  no threshold, no colour beyond a neutral bar. Acting on it is lot 6.
- **The budget carries no currency of its own**; it is read in the currency the cost rows come back
  in. A period that contains more than one currency is shown as one line per currency and is never
  totalled, because adding euros to dollars produces a number that is wrong in a way nobody
  notices.
- When `azure_mode` is `off`, the page explains what to ask an administrator for, in one short
  paragraph, rather than showing an empty table.

A **Azure** tab in Settings holds the configuration above.

---

## 10. What this lot proves, and what it does not

Everything is tested against a local fake: the token endpoints, ARM pagination, `429` with
`Retry-After`, `403`, the three token response shapes, the columnar cost response, the soft delete,
and the property that a failed sync leaves the previous inventory intact.

**The lowercase join is load-bearing and easy to test vacuously.** Observed on 2026-09-18: cost rows
come back fully lowercased (`resourcegroups`, `microsoft.web`) while ARM returns `resourceGroups`
and `Microsoft.Web`. A test whose two fixtures are already lowercase proves nothing. The inventory
fixture is built from a real mixed-case ARM id, so removing the normalisation fails the test.

**It does not prove that the credential works**, because no credential exists yet. The first real
sync is Vincent's, once an administrator has acted, and the **Test connection** button exists so
that it takes one click and returns Azure's own words. The plan says this rather than letting a
green suite imply more than it proves — the same honesty lot 2 applied to Microsoft Teams.
