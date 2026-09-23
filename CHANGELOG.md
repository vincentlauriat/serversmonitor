# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Cost guardrails (lot 6), evaluated only after a successful sync and delivered through the
  existing alert channels, on an append-only journal of their own (`azure_guardrail_events`) that
  the `deliveries` table can now point at instead of `alert_events`.
  - Budget alerts: two configurable thresholds on month-to-date spend, an end-of-month projection
    with a 5 % hysteresis band that does not compute before the 4th billed day, and a per-resource
    share alert. All currencies are summed; a resource deleted mid-month still counts.
  - Orphan detection: five typed checks per inventory sweep — an unattached disk, an unassociated
    public IP, a NIC with no VM, an App Service plan with no sites, and a hub-created VM whose agent
    has gone quiet. A resource Azure refuses to read in detail is `unverified`, never reported
    healthy, and can never be deleted from the page.
  - Stop schedules: off windows per resource in one hub-wide time zone, acted on only when a window
    boundary is crossed — never by continuous enforcement — with bounded catch-up (under twelve
    hours) at startup and no retry on a failed scheduled action.
  - Orphan deletion by resource name, generalising the lot 5 confirm-by-name rule from VMs to any
    deletable ARM resource.
  - `GET /api/v1/azure/guardrails`, `GET`/`PUT /api/v1/azure/guardrails/settings`,
    `GET`/`PUT /api/v1/azure/schedules`, `POST /api/v1/azure/schedules/delete`,
    `POST /api/v1/azure/orphans/delete`.
  - Azure page: a Budget block with threshold marks and projection, an Orphans block, a schedule
    column and per-resource editor on the resource table, an Origin column on recent actions, and a
    Guardrails block under Settings → Azure.

### Fixed
- `install.sh` no longer writes `SupplementaryGroups=docker` into the agent's systemd unit. On a
  machine without Docker the group does not exist and systemd refuses to start the service
  (`status=216/GROUP`), which is why the first VM the hub created never connected. The agent runs
  as root and needs no group to read the Docker socket.
- A VM `PUT` answered with `201 Created` and an `Azure-AsyncOperation` header is now followed to
  its end; it was recorded as succeeded two seconds after the request, while Azure was still
  building the machine.

### Changed
- Default VM size for provisioning is `Standard_B2ats_v2` instead of `Standard_B1s`: the first
  real provisioning attempt failed with `SkuNotAvailable`, and no B-series v1 size is offered to
  the sandbox's subscription in West Europe.

## [0.5.0] — 2026-09-22

First tagged release. Lots 1 to 5 are in; lot 6 (cost guardrails) is not.

### Added
- Hub (`smhub`) and agent (`smagent`), Go only, front SvelteKit embedded in the hub binary.
- Agent metrics: CPU, memory, disks, network, temperatures, Docker containers; outbound WebSocket
  with a per-host token; `install.sh` served by the hub.
- Alert rules with pending → firing → resolved transitions, host mute, an append-only event log.
- Alert channels: SMTP, generic webhook / ntfy, Microsoft Teams (Adaptive Card), with a delivery
  log and at-least-once delivery.
- Azure read (Reader role): inventory, per-resource state, month-to-date costs against a budget,
  deleted-but-billed resources kept as their own rows. Client secret or managed identity.
- Azure actions (Website Contributor role): start, stop, restart of App Services, with the state
  re-read from Azure afterwards; interrupted actions are never replayed.
- VM provisioning (Virtual Machine Contributor role): a Linux VM in an existing subnet with the
  agent installed by cloud-init, every created resource recorded, deletion confirmed by name.
- Deployment recipe for an Azure VM with Docker and a separate data disk (`deploy/azure/`).

### Release assets
- `smhub-<os>-<arch>` and `smagent-<os>-<arch>` for linux/amd64, linux/arm64, darwin/amd64,
  darwin/arm64. `install.sh` downloads `smagent-<os>-<arch>` from `releases/latest/download/`.

[0.5.0]: https://github.com/vincentlauriat/serversmonitor/releases/tag/v0.5.0
