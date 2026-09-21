# Running the hub in an Azure resource group

What was actually deployed on 2026-09-21, and why each choice was made. Everything here was run
as a user who is **Contributor on the resource group and nothing more** — no subscription-level
right, no ability to assign a role.

## Why a VM, and not App Service or Container Apps

The hub opens SQLite with `journal_mode(WAL)` (`internal/hub/store/store.go`). WAL needs a `-shm`
file, which is shared memory mapped from disk. Every managed container service on Azure offers
persistence through Azure Files, which is SMB, and SMB does not provide that. The failure would
not be a clean refusal — it would be a database that misbehaves under concurrency.

Checked rather than assumed: running the image locally with a volume produces
`serversmonitor.db`, `serversmonitor.db-wal` **and `serversmonitor.db-shm`**.

| Option | Persistent volume | SQLite WAL |
|---|---|---|
| App Service for Containers | `/home`, Azure Files | no |
| Container Instances | Azure Files | no |
| Container Apps | Azure Files | no |
| Container Apps, no volume | container disk, ephemeral | works, lost on restart |
| **Docker on a VM** | managed disk, ext4 | **works** |

A free (F1) App Service plan is doubly unsuitable: no Always On, so the hub sleeps and the agents'
WebSockets drop.

## Shape of the deployment

```
vnet-sandbox / default (10.0.1.0/24)
  └── nic-serversmonitor (10.0.1.4) ── pip-serversmonitor (Standard, static)
        └── vm-serversmonitor (Ubuntu 24.04, system-assigned managed identity)
              ├── osdisk-serversmonitor    30 GB  deleteOption: Delete
              └── datadisk-serversmonitor  32 GB  deleteOption: Detach   <- the data
                    mounted at /var/lib/serversmonitor, by LABEL
                      └── docker run --restart=always -v /var/lib/serversmonitor:/data
```

**The data disk is separate and `Detach`.** The VM is disposable; the metrics are not. Destroying
and recreating the VM on the same disk finds the database as it was — `overwrite: false` in
cloud-init is what guarantees it.

**Mount by label, never by device name.** On the first reboot the data disk moved from `sdb1` to
`sda1`. This is not theoretical; it happened.

**Do not tag this VM `createdBy=ServersMonitor`.** That is the tag the hub's own delete path
accepts, so the hub would be willing to delete the machine it runs on. It is tagged
`role=serversmonitor-hub`.

## Building and shipping the image

There is no registry, and none is needed: an ACR Basic would be €4.29/month for one image.

```bash
# On a Mac, --platform is not optional: the default build is arm64 and the VM is x86_64.
docker buildx build --platform linux/amd64 -f deploy/Dockerfile \
  --build-arg VERSION=$(git rev-parse --short HEAD) \
  -t serversmonitor/hub:$(git rev-parse --short HEAD)-amd64 --load .

docker save serversmonitor/hub:<tag>-amd64 | gzip -9 > /tmp/hub.tar.gz   # about 5 MB
scp /tmp/hub.tar.gz azureuser@<public-ip>:/tmp/
ssh azureuser@<public-ip> 'sudo docker load -i /tmp/hub.tar.gz && rm /tmp/hub.tar.gz'

ssh azureuser@<public-ip> 'sudo docker run -d --name serversmonitor --restart=always \
  -p 8090:8090 -v /var/lib/serversmonitor:/data serversmonitor/hub:<tag>-amd64'
```

Upgrading is `docker rm -f serversmonitor` then the same `docker run` with the new tag. The data
disk is untouched.

## Network posture

Nothing is exposed to the internet. The NSG allows exactly two things:

| Priority | From | Port | For |
|---|---|---|---|
| 100 | the operator's own address | 22 | SSH, and the tunnel to the UI |
| 200 | `VirtualNetwork` | 8090 | agents on other VMs in the VNet |

The UI is reached through the tunnel, so no password crosses the internet in clear text:

```bash
ssh -N -L 8092:127.0.0.1:8090 azureuser@<public-ip>   # then http://127.0.0.1:8092
```

The alternative, if browser-native access matters more than the extra moving part, is a TLS
reverse proxy on the same VM with a `*.cloudapp.azure.com` DNS label. The application has no TLS
of its own, so opening 8090 to the internet without one would send the login password in clear.

## The credential

The VM carries a **system-assigned managed identity**, which is what the hub's
`managed_identity` mode expects. Creating it needs only a write on the VM, which Contributor has —
unlike an app registration, which this tenant blocks for standard users
(`allowedToCreateApps: False`).

That removes the client secret entirely, but **not** the administrator. The identity still has no
role, so every ARM call answers 403 until someone with `Microsoft.Authorization/roleAssignments/write`
runs:

```bash
RG="/subscriptions/<sub>/resourceGroups/<rg>"
PRINCIPAL="<the VM's identity principalId>"
for ROLE in "Reader" "Website Contributor" "Virtual Machine Contributor"; do
  az role assignment create --assignee-object-id "$PRINCIPAL" \
    --assignee-principal-type ServicePrincipal --role "$ROLE" --scope "$RG"
done
```

`Reader` covers inventory *and* cost: the Cost Management query is an HTTP POST whose operation is
`Microsoft.CostManagement/query/read`.

## Two traps worth knowing before you start

**`az vm create` can hide the real error.** Three attempts failed with a CLI traceback
(`RuntimeError: The content for this response was already consumed`) that says nothing about the
cause. Issuing the VM as a direct ARM `PUT` returns Azure's own message — in that case
`SkuNotAvailable` for `Standard_B1s` in West Europe, a capacity restriction with nothing to do with
the request. `az vm list-skus -l <region> --all` shows what is actually available.

**Check the region of the resources, not of the group.** `rg-dev-vincent-sandbox` is declared in
`northeurope` while every resource in it lives in `westeurope`. A VM must sit in the same region as
its VNet.

## What it costs

Retail prices for West Europe in EUR, read from the Azure pricing API on 2026-09-21:

| | €/month |
|---|---|
| `Standard_B2ats_v2` (2 vCPU, 1 GB), 24/7 | 12.56 |
| Static Standard public IP | 3.80 |
| OS disk, 30 GB StandardSSD | 2.06 |
| Data disk, 32 GB StandardSSD | 2.06 |
| **Total** | **≈ 20.50** |

`Standard_B1s` would have been 7.52 and is the obvious choice when capacity allows it.
