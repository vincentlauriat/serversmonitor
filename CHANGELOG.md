# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed
- The Network and Disk I/O charts labelled every tick in MB/s with one decimal, so a few KB/s read
  as a column of "0.0 MB/s". The axis now picks one unit (B/s to GB/s) from its largest tick, steps
  in whole binary units, and the hover values use the same readable format as the page header.

### Added
- Several people (lot 11). Sign-in with Microsoft Entra ID (OIDC code flow with PKCE, ID token
  signature, issuer, audience, tenant, expiry and nonce all checked), for addresses an admin lists
  under Settings → Users; two roles, admin and read-only, the latter refused any change by the API
  itself; the local account made at setup stays as the way in when Entra is down, and cannot be
  removed or demoted while it is the last local admin. Migration 10 (`users.role`,
  `users.provider`); `/api/v1/users`, `/api/v1/auth/entra/settings`, `/start`, `/callback`.

### Security
- The password route no longer runs an account without a password against the dummy hash kept for
  timing: before this, a password-less account would have accepted the word used to build that
  hash. No such account could exist before lot 11; Entra accounts are the first.

- Native TLS (lot 10). The hub can serve HTTPS itself, from a certificate on disk
  (`SM_TLS_CERT`, `SM_TLS_KEY`), re-read when the files change, or from Let's Encrypt
  (`SM_TLS_DOMAINS`, `SM_TLS_EMAIL`, `SM_ACME_DIRECTORY`), with TLS-ALPN-01 on the HTTPS port and
  the cache in the data directory. With TLS on, the hub listens on `:443`, a plain listener on
  `:80` (`SM_HTTP_LISTEN`, `off` to disable) redirects to HTTPS and answers HTTP-01, and cookies
  are always Secure. Without these variables nothing changes. New dependency: `golang.org/x/net`,
  through `golang.org/x/crypto/acme/autocert`.

## [0.7.0] — 2026-09-28

Lots 7, 8 and 9 are in. No new Azure role is needed; the hub applies database migrations 7 to 9 by
itself on first start, and every existing rule and schedule behaves as it did: rules and the offline
and guardrail routes go to every enabled channel, schedules follow the hub-wide time zone.

One API detail to know when scripting: `PUT /api/v1/notifications` without `offline_channels` or
`guardrail_channels` sets that route back to every channel, since absent means `null`. The page
always sends both.

### Added
- A time zone per stop schedule (lot 9). A schedule can carry its own IANA zone; without one it
  follows the hub-wide zone, read at each tick, which is what every existing schedule keeps doing
  after migration 9. An unknown zone, or `Local`, is refused on save. `timezone` and
  `effective_timezone` on `/api/v1/azure/schedules`; the schedule editor has a zone field with the
  browser's zones as suggestions.
- Outbound access check for new VMs (lot 8). The hub reads the configured subnet's outbound access
  from the VNet read it already makes (NAT Gateway, `defaultOutboundAccess`, route table), with no
  new role. It is shown next to Create VM, recorded on each run (migration 8), and a VM whose agent
  has not called in ten minutes after creation says so, naming the subnet when it had no way out.
  It warns and never refuses. `GET /api/v1/azure/vms/outbound`; provisions gain `outbound`,
  `outbound_detail` and `host_status`.
- Alert routing per rule (lot 7). Each rule picks the channels it notifies: every enabled channel
  (the default, and what every existing rule keeps doing after migration 7), some of them, or none,
  in which case the alert still fires and shows but tells nobody. The offline rule and the Azure
  guardrails get the same choice under Settings → Notifications → Routing.
  - A `resolved` goes wherever its `fired` went, plus the current route, so re-routing a rule while
    it fires never leaves a channel with an alert that does not end.
  - A route naming a channel that is switched off is kept and skipped; the rules table marks it
    `(off)`.
  - `channels` on the rule endpoints, `offline_channels` and `guardrail_channels` on
    `/api/v1/notifications`: `null` for every channel, `[]` for none.

## [0.6.0] — 2026-09-28

Lot 6 (cost guardrails) is in. No new Azure role is needed; the hub applies database migration 6
by itself on first start.

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

[0.7.0]: https://github.com/vincentlauriat/serversmonitor/releases/tag/v0.7.0
[0.6.0]: https://github.com/vincentlauriat/serversmonitor/releases/tag/v0.6.0
[0.5.0]: https://github.com/vincentlauriat/serversmonitor/releases/tag/v0.5.0
