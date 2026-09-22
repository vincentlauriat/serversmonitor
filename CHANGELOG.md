# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
