# Lot 6 — deploying it, then proving it

Two things Vincent runs by hand, because the classifier refuses both to Claude: deploying the new
image to `vm-serversmonitor`, and the two real checks against the sandbox subscription.

**Before any SSH step: check the NSG.** Vincent's public IP changes often, and the NSG rule
`allow-ssh-from-vincent` on `vm-serversmonitor` only admits the address it was last set to. If a
step below refuses to connect, that is the first thing to check — not the VM.

## Deploying lot 6

The hub on `vm-serversmonitor` is currently running image `a3f1a1c-amd64`, at **schema version 5**.
Migration 6 (`internal/hub/store/migrations/0006_guardrails.sql`) has only ever been applied to a
copy of the database, on the controller's own machine — nothing in this lot deploys anything. The
first time it touches the real database is whenever this section is run, and it rebuilds the
`deliveries` table in one transaction alongside five smaller schema changes, on the one machine that
holds Vincent's admin account and the whole alert history. Treat it as a decision, not a side effect
of a container restart.

1. **Build and ship the new image**, the same recipe as the rest of `deploy/azure/README.md`:

   ```bash
   docker buildx build --platform linux/amd64 -f deploy/Dockerfile \
     --build-arg VERSION=$(git rev-parse --short HEAD) \
     -t serversmonitor/hub:$(git rev-parse --short HEAD)-amd64 --load .
   docker save serversmonitor/hub:$(git rev-parse --short HEAD)-amd64 | gzip -9 > /tmp/hub.tar.gz
   scp /tmp/hub.tar.gz azureuser@20.101.76.155:/tmp/
   ```

   Note the tag printed here (`$(git rev-parse --short HEAD)-amd64`) — it is used below as
   `<new-tag>`, and it must **not** be loaded or started yet.

2. **Stop the running container.** Do not remove it and do not remove the old image — both are the
   rollback.

   ```bash
   ssh azureuser@20.101.76.155 'sudo docker stop serversmonitor'
   ```

3. **Copy `serversmonitor.db` off the VM, before anything else touches it.** This is the only copy
   of the admin account and the alert/guardrail history. Do this before loading the new image, not
   after — a mistake in step 4 or 5 must still leave an untouched copy on the Mac.

   Save it **outside this repository** — it holds the admin password hash, session tokens and the
   whole alert/guardrail history, and nothing about it should ever end up in a `git add`:

   ```bash
   mkdir -p ~/serversmonitor-backups
   ssh azureuser@20.101.76.155 'sudo cp /var/lib/serversmonitor/serversmonitor.db /tmp/serversmonitor-pre-lot6.db && sudo chmod a+r /tmp/serversmonitor-pre-lot6.db'
   scp azureuser@20.101.76.155:/tmp/serversmonitor-pre-lot6.db ~/serversmonitor-backups/serversmonitor-pre-lot6-$(date +%Y%m%d).db
   ssh azureuser@20.101.76.155 'sudo rm -f /tmp/serversmonitor-pre-lot6.db'
   ```

   Confirm the file arrived and is not empty before continuing:

   ```bash
   ls -la ~/serversmonitor-backups/serversmonitor-pre-lot6-$(date +%Y%m%d).db
   ```

4. **Load the new image on the VM.**

   ```bash
   ssh azureuser@20.101.76.155 'sudo docker load -i /tmp/hub.tar.gz && rm /tmp/hub.tar.gz'
   ```

5. **Start it.** This is the moment migration 6 runs, against the real database, inside the
   container's first `store.Open`.

   ```bash
   ssh azureuser@20.101.76.155 'sudo docker rm -f serversmonitor && sudo docker run -d --name serversmonitor --restart=always \
     -p 8090:8090 -v /var/lib/serversmonitor:/data serversmonitor/hub:<new-tag>'
   ```

6. **Confirm the schema actually moved to 6.** `smhub` does not log a schema line at startup
   (checked in `cmd/smhub/main.go` and `internal/hub/store/store.go` — there is none to grep for),
   so read `schema_version` back directly rather than trusting a log line that does not exist:

   ```bash
   ssh azureuser@20.101.76.155 'command -v sqlite3 >/dev/null || { sudo apt-get update && sudo apt-get install -y sqlite3; }'
   ssh azureuser@20.101.76.155 'sudo sqlite3 /var/lib/serversmonitor/serversmonitor.db "select max(version) from schema_version;"'
   ```

   Expected: `6`. Also worth a look, since it is the whole reason for step 3:

   ```bash
   ssh azureuser@20.101.76.155 'sudo sqlite3 /var/lib/serversmonitor/serversmonitor.db "select count(*) from deliveries;"'
   ```

   compared against the same query run on the copy saved in step 3
   (`sqlite3 ~/serversmonitor-backups/serversmonitor-pre-lot6-<date>.db "select count(*) from deliveries;"`
   — the raw file, read with plain `sqlite3`, needs no particular binary to query, and at schema 5
   that table still has only `event_id`, no `guardrail_event_id` column). The counts should match:
   the rebuild must not have lost a row.

7. **Confirm the hub is otherwise healthy.**

   ```bash
   ssh azureuser@20.101.76.155 'sudo docker logs --tail 50 serversmonitor'
   ssh -N -L 8092:127.0.0.1:8090 azureuser@20.101.76.155 &   # then open http://127.0.0.1:8092
   ```

   Log in, and check the Azure page loads with its three new blocks (Budget, Orphans, the schedule
   column) — Azure is already configured, so this does not need a fresh sync to render.

### Rollback

The old image (`a3f1a1c-amd64`) is still in `docker images` on the VM — `docker load` never removes
an existing image, only adds one. But **a database migrated to schema 6 will not open on the old
binary**: the old code does not know about `guardrail_event_id`, `azure_schedules`, or the columns
migration 6 added to `azure_resources` and `azure_actions`. Rolling back the image alone would start
a binary that cannot read its own database. Rolling back means both:

```bash
ssh azureuser@20.101.76.155 'sudo docker rm -f serversmonitor'
scp ~/serversmonitor-backups/serversmonitor-pre-lot6-<date>.db azureuser@20.101.76.155:/tmp/rollback.db
ssh azureuser@20.101.76.155 'sudo cp /tmp/rollback.db /var/lib/serversmonitor/serversmonitor.db && sudo rm /tmp/rollback.db'
ssh azureuser@20.101.76.155 'sudo docker run -d --name serversmonitor --restart=always \
  -p 8090:8090 -v /var/lib/serversmonitor:/data serversmonitor/hub:a3f1a1c-amd64'
```

This discards anything written between the deploy and the rollback (new hosts, new alerts, new
deliveries) — the same trade-off as restoring any backup taken before a migration. There is no
partial rollback available once step 5 above has run.

---

## Proof 1 — a stop schedule really stops and starts the site, as the hub

Schedule windows are weekly (`days` + `HH:MM` in the hub's zone), not one-off timestamps, so compute
today's day number and the next few minutes before opening the editor — **in the hub's own zone**
(`Europe/Paris` by default, `Settings → Azure` says which): if the Mac's own zone differs, prefix
every command below with `TZ=Europe/Paris`. These are the macOS (BSD) `date` flags, not GNU's `-d`:

```bash
date +"day=%u from=%H:%M"      # now, in the hub's zone — %u is ISO day numbering, 1 = Monday,
                                # matching off_windows[].days exactly
date -v+5M +"from=%H:%M"       # window start, five minutes out
date -v+15M +"to=%H:%M"        # window end (a 10-minute window)
```

1. Through the SSH tunnel from step 7 above, open the Azure page, find
   `webapp-vlauriat-probe-09121238`, open its schedule editor, and set a single window: the day from
   `date +%u`, `from` = the start time, `to` = the end time printed above. Save it, and leave the
   browser tab open — no further action needed, the hub's minute tick does the rest.
2. Wait until the window has both started and ended (about fifteen minutes from when you ran the
   `date` commands).
3. Confirm the calls actually happened, and that they came from the hub, not a person:

   ```bash
   RID=$(az webapp show -g rg-dev-vincent-sandbox -n webapp-vlauriat-probe-09121238 --query id -o tsv)
   az monitor activity-log list --resource-id "$RID" \
     --start-time "$(date -u -v-30M +%Y-%m-%dT%H:%M:%SZ)" \
     --query "[?contains(operationName.value, 'sites/stop') || contains(operationName.value, 'sites/start')].{op:operationName.value, status:status.value, caller:caller, time:eventTimestamp}" \
     -o table
   ```

   Expected: exactly two rows, `Microsoft.Web/sites/stop/Action` then
   `Microsoft.Web/sites/start/Action`, both `Succeeded`, roughly ten minutes apart, `time` matching
   the window — not three. (Checked against `LastBoundary` in
   `internal/hub/guardrails/schedule.go`: a fresh schedule with a single day selected has no
   boundary from "last week" inside the lookback window it searches, so the very first minute tick
   after Save sees no boundary at all and does nothing; the stop fires only once the window's start
   is reached.) Then confirm the `caller` is the hub, not a person at the CLI or in the portal:

   ```bash
   az vm identity show -g rg-dev-vincent-sandbox -n vm-serversmonitor --query principalId -o tsv
   ```

   The GUID this prints (the same identity granted Reader / Website Contributor / Virtual Machine
   Contributor on 2026-09-22) must equal the `caller` column above.

4. On the Azure page, the resource table's Recent actions section should show both actions with
   `origin: schedule` — confirming the same distinction from the database side, not only Azure's.

---

## Proof 2 — an orphan is detected, shown with no cost, and deleted by name

```bash
az disk create -g rg-dev-vincent-sandbox -n sm-orphan-proof --size-gb 4 --sku Standard_LRS
```

This disk is attached to nothing, so it is `disk_unattached` from the moment the next inventory
sweep sees it.

1. Wait for the next sweep — up to `azure_inventory_interval_min` (15 minutes by default,
   `internal/hub/azure/config.go`) — or trigger one immediately by re-saving the Azure settings
   unchanged under **Settings → Azure** (the architecture doc's "kick": saving syncs now rather
   than waiting for the next tick).
2. On the Azure page's Orphans block, `sm-orphan-proof` should appear with reason
   `disk_unattached`, a `since` timestamp from just now, and cost shown as **—**, never `0.00` — a
   disk Azure has not billed yet is not a disk that costs nothing, it is a disk that has not been
   billed.
3. Before the real delete, worth trying once to see the refusal work: press **Delete** and type a
   name that does *not* match (`sm-orphan-prof`, or leave the field empty). The page must refuse
   locally and send nothing to Azure — no request leaves the browser until the typed name matches
   the resource's exactly, so there is no Azure error to show, only the page's own.
4. Press **Delete** again, and this time type `sm-orphan-proof` exactly to confirm.
5. Confirm Azure agrees it is gone:

   ```bash
   az disk show -g rg-dev-vincent-sandbox -n sm-orphan-proof
   ```

   Expected: `ResourceNotFound` (the CLI reports it as an error — that is success here).
