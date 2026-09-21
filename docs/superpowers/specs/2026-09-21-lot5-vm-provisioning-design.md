# Lot 5 — VM provisioning, with the agent already installed

**Goal.** Create a Linux VM in the sandbox from the ServersMonitor interface, with the agent
installed and reporting before anybody logs into it. Delete it again when it is no longer wanted.

**Stated up front.** This is the **third Azure lot built without ever having reached Azure**. Lot 3
never read a subscription, lot 4 never started an App Service, and lot 5 will never have created a
VM until a tenant administrator assigns a role once. Building it anyway is a decision, taken
knowingly; what it costs is that the lot ships with more unproven surface than the last one, and the
first real run will find things no fake can.

---

## 1. What the role actually allows, checked against the tenant on 2026-09-21

Not recalled — read back from the live role definition
(`az role definition list --name "Virtual Machine Contributor"`):

| The hub can | The hub cannot |
|---|---|
| `Microsoft.Compute/disks/*`, `Microsoft.Compute/virtualMachines/*` | — |
| `Microsoft.Network/networkInterfaces/*` — create the NIC | `Microsoft.Network/virtualNetworks/write` — **no VNet** |
| `virtualNetworks/read`, `subnets/join/action` — use an existing subnet | `publicIPAddresses/write` — **no public IP** (only `join` and `read`) |
| `networkSecurityGroups/join/action`, `read` | `networkSecurityGroups/write` — **no NSG** |

**Two consequences, and they shape the whole lot.**

**The hub never creates a network.** A VNet with a subnet has to exist first. Vincent can create it
himself — he is Contributor on the resource group, and only *role assignment* is beyond him — so
this is a one-off he controls, not another wait on an administrator. The subnet id goes in the
settings, the hub reads it and joins it, and refuses to provision when it is empty rather than
inventing one.

**The VM gets no public IP, and does not need one.** This is not a workaround for a missing
permission; it is the lot 1 decision paying off. **The agent dials out** over an outbound WebSocket,
so a VM with no inbound address is exactly as monitorable as one with a public address — and it is
one fewer thing exposed to the internet. Nobody can SSH to it from outside, which is a property to
state plainly rather than discover.

What it does require is the reverse: **the VM must be able to reach the hub.** A hub on
`localhost:8091` on a Mac is not reachable from Azure, so provisioning is only useful once the hub
has an address the VM can resolve. The settings carry that address explicitly instead of the hub
guessing its own, and the page says so where the button is.

Verified the same day: **10 regional vCPUs available in West Europe, 0 in use**, and no policy
assignment restricting sizes. A B-series VM fits with room to spare.

---

## 2. The prerequisite lot 4 under-promised

Lot 4's spec said adding VMs would be "a row, not a redesign". That was half right. The row is real —
`Microsoft.Compute/virtualMachines` maps to `start` / `deallocate` / `restart` — but **those calls
answer 202 with an `Azure-AsyncOperation` header, and the client throws headers away.** `attempt`
returns `([]byte, time.Duration, error)`; the response headers never reach `Do`.

So the first task of this lot is the client, again, and named as such:

- `attempt` and the call path return the response **headers and status**, not only the body.
- A 202 starts a poll of `Azure-AsyncOperation` (falling back to `Location`), honouring
  `Retry-After`, until the operation reports `Succeeded`, `Failed` or `Canceled`.
- **The `running` state in `azure_actions` is finally driven by something.** Until now every action
  went `pending` → `running` → finished within one call; with a VM it means what it says.
- A poll that is still running when the hub stops leaves the row `running`, and the startup rule
  turns it into `interrupted` — unchanged, and now it matters: **a VM creation that was interrupted
  may have created resources.** See §4.

---

## 3. Creating a VM is several calls, and the record says so

The order, each a `PUT`:

1. **NIC** — joined to the configured subnet, no public IP.
2. **VM** — image, size, admin user, SSH public key, OS disk, and `customData` (§5). The OS disk is
   created implicitly by the VM `PUT`; it is not a separate call, and it *is* separately billed.

Every resource the hub creates carries the tag **`createdBy=ServersMonitor`**, and the VM also
carries `smHostId=<host row id>`. Lot 6 needs to find orphans; a tag it can trust is what makes that
possible without guessing from names.

Provisioning is recorded in a new table `azure_provisions`, written before the first call, with one
row per created resource as it lands. Not a reuse of `azure_actions`: an action is one call on one
existing resource, and a provision is several calls creating several.

---

## 4. Partial creation: the hub records orphans, it does not roll them back

A provisioning run that dies after the NIC and before the VM leaves a resource behind. Two honest
answers exist; this lot takes the second, deliberately:

- *Roll back* — delete what was created. Rejected. **Deleting is the one thing lots 1 to 4 never
  do**, and putting the first automatic delete in the failure path — the least exercised path in the
  lot — is how a hub deletes something it should not have. The failure path is also exactly where
  the hub's view of what it created is least trustworthy.
- **Record and show.** The provision row keeps every resource it created and its state; the page
  shows a failed provision with its leftovers named, and a **Delete** button beside each one that
  the user presses. Deleting stays a thing a person decides, every time.

This is the same shape as lot 4's `interrupted`: the hub says what it knows and does not act on a
guess. The difference is that here the leftovers cost money, so they are shown on the Azure page
next to the costs, not buried in a log.

---

## 5. Cloud-init, and the token it carries

`customData` holds a cloud-init document that installs the agent and points it at the hub. It
therefore **contains a host token in clear text**, and that has to be stated rather than glossed:

- The token is **per host and revocable** (`POST /api/v1/hosts/{id}/token` already exists). Its blast
  radius is one host's metrics, and rotating it is one click.
- ARM does not return `customData` on a read, and the hub creates the VM with a direct `PUT` rather
  than a deployment template, so it does not land in deployment history either. Anyone who can log
  into the VM can read it at `/var/lib/cloud`, which is the same trust boundary as the agent binary
  itself.
- It is **never logged, never echoed by the API, and never shown twice** — the same rule the SMTP
  password follows. The provision row stores the host id, not the token.

The hub creates the host row *first*, so that a VM that comes up and dials in finds itself expected.
A provision that fails leaves a host row that never connected, visible as such.

---

## 6. Deleting

In scope, because a sandbox that can only grow is a cost problem, and because it was asked for at
the start ("créer/supprimer des VM").

Delete removes the VM, then its OS disk, then its NIC — in that order, because Azure refuses to
delete a disk or NIC still attached. It is **always user-initiated and always confirmed by typing
the VM's name**, not by a dialog anyone dismisses by reflex. The hub deletes only resources tagged
`createdBy=ServersMonitor`; anything else it refuses, so a mis-typed id cannot reach a resource a
person created by hand.

The matching host row is *not* deleted automatically: its history is the record of a machine that
existed. The page offers it as a separate, explicit action.

---

## 7. Settings, API and page

New settings: subnet id, VM size (default `Standard_B1s`), image (default Ubuntu 24.04 LTS), admin
username, SSH public key, and **the hub address the agent should dial**. Provisioning is refused,
with a message naming what is missing, when the subnet or the hub address is empty.

```
POST /api/v1/azure/vms      {"name":"…","size":"…"}    → 202 {"provision_id": 4}
GET  /api/v1/azure/vms                                  → provisions and their resources
POST /api/v1/azure/vms/delete  {"resource_id":"…","confirm_name":"…"} → 202
```

The page gets a **New VM** form and a provisions list showing each run, its resources, and its
error. A failed run shows its leftovers with their own Delete buttons.

---

## 8. What tests can prove, and what they cannot

Provable against a fake ARM: the call order, the 202 poll including `Retry-After` and a `Failed`
terminal state, the tag on every created resource, the refusal when the subnet or hub address is
missing, the orphan record after a mid-run failure, delete ordering, the refusal to delete an
untagged resource, the name-confirmation, and that the token never appears in any response or log.

**Not provable until a tenant administrator acts:** that a VM was ever created, that cloud-init ran,
or that an agent installed this way ever dialled in. That last one is the real point of the lot, and
it stays unproven. It goes into `TODOS.md` → "À prouver hors tests" before the first line of code,
not after.

**The admin ask, corrected for the third time** — and this is the version that unblocks lots 3, 4
and 5 together, on `rg-dev-vincent-sandbox`:

> **Reader** + **Website Contributor** + **Virtual Machine Contributor**

plus one thing Vincent does himself, no administrator needed: **create a VNet with a subnet** in
that resource group.
