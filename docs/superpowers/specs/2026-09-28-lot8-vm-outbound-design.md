# ServersMonitor — Lot 8: warn when a new VM's subnet cannot reach the internet

**Date**: 2026-09-28
**Status**: design, choices made by Vincent on 2026-09-28
**Scope**: this repository. Lot 5 creates VMs; lot 8 says, before and after, whether they can call in.

## 1. What is missing

The README said: *if `defaultOutboundAccess` is false and there is no NAT Gateway, the VM boots and
the agent never connects, with nothing on the hub's side to say why.* It happened: the sandbox
subnet was created on 2026-09-21 with `defaultOutboundAccess: false`, because Azure retired
implicit outbound access for new deployments on 2025-09-30.

A VM the hub creates needs a way out twice: cloud-init fetches the agent from the GitHub release,
then the agent dials the hub. Being in the same VNet as the hub does not help the first.

## 2. What the role allows, re-read

The README claimed that reading the subnet was outside the role. It is not. `Virtual Machine
Contributor` grants `Microsoft.Network/virtualNetworks/read`, and a VNet `GET` carries its subnets
inline, with `defaultOutboundAccess`, `natGateway` and `routeTable`. The hub already makes that
`GET` to find the region. **Checking needs no new role; fixing still does** (a `Microsoft.Network`
write), so the hub reports and never repairs.

## 3. The verdict

Read in this order, because this is the order in which Azure picks a path:

| Subnet | State | Reaches? |
|---|---|---|
| a NAT Gateway | `nat` | yes |
| a route table | `route_table` | unknown: a `0.0.0.0/0` to a firewall wins, and the firewall is not readable |
| `defaultOutboundAccess` absent | `legacy` | yes, a subnet from before 2025-09-30 |
| `defaultOutboundAccess: true` | `default` | yes, deprecated |
| `false`, none of the above | `none` | no |
| subnet not in the read, or unreadable | `unknown` | unknown |

## 4. The choice

Asked of Vincent: when the subnet clearly has no way out, does Create VM refuse or warn?
**Warn, and create anyway.** A path the hub cannot see (a route table, a peering) may still lead
out, and the person pressing the button knows their network better than one `GET`.

## 5. Where it shows

1. **Before**: `GET /api/v1/azure/vms/outbound` reads the configured subnet live. The Azure page
   shows the verdict under the Create VM form, in amber when it does not reach out.
2. **During**: the run records what it read (`azure_provisions.outbound`, `outbound_detail`,
   migration 8) as soon as the VNet is read, before the NIC. A run that dies at the NIC still says it.
3. **After**: provisions carry `host_status`. A run that succeeded more than ten minutes ago whose
   host is still `never_seen` gets a sentence: created N minutes ago, agent has not called in, and
   the subnet's own sentence when it had no way out, otherwise a pointer to GitHub and the hub
   address. The page ticks every minute, since silence is exactly the case where no event comes.

## 6. Tests that must exist

- every row of the table in §3, and case-insensitive subnet ids;
- `CreateVM` reports the verdict and still sends both `PUT`s when there is no way out;
- the hub records it against the run, and the live check only ever sends `GET`s;
- the API maps Azure off and a missing subnet to 400, an Azure refusal to 502;
- the front says nothing before ten minutes, nor for a failed run or a host that called in.

## 7. Out of scope

Following a route table to its next hop, or reading peerings. Checking that the hub address itself
is reachable from the subnet. Any repair of the subnet.
