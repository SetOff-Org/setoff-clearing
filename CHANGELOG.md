# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- On-chain settlement from the service: with `[settlement]`, closed windows
  are simulated against the settlement contract, debtors fetch and return
  their Soroban authorization entries over
  `/v1/windows/{n}/authorizations`, and the service submits each batch and
  settles. Signed entries are checked byte for byte against the issued ones
  and account signatures are verified before they are recorded.
- `setoff authorize`: a debtor signs its authorizations with its own key.
- `scripts/testnet-settle.sh`: the whole flow against a fresh testnet contract.

## [0.1.0] - 2026-10-07

### Added

- `setoff net`: multilateral netting of CSV or JSON obligations, reproducing
  every setoff-engine reference vector exactly.
- `setoff serve`: a clearing service with per-participant keys, a journaled
  open window, live netting, and archived windows.
- `GET /v1/me` (a participant's own side of the window) and `GET /v1/windows`
  (archived windows, newest first).
- ISO 20022 camt.053 statements per participant and asset, over the API and
  with `setoff report`.
- `setoff soroban`: the settlement-contract calls that settle a closed window,
  submitting the netted plan with deterministic references.
- `close_every` to close windows on a schedule, and signed, retried webhooks
  on every close.
- SEP-10 sign-in: participants authenticate with their Stellar accounts and
  get short-lived bearer tokens; `/.well-known/stellar.toml` publishes the
  signing key.
- An optional asset allowlist.
- Identical resubmissions return the original obligation instead of a 409.
- Request IDs, structured access logs and Prometheus metrics by route.
- OpenAPI 3.1 description in `api/openapi.yaml`, checked against the routes.
- Container image, release binaries, and CI on Linux, macOS and Windows with
  govulncheck.

### Fixed

- A crash while closing a window could replay the archived window's
  obligations into the next one, settling them twice.
- A write torn by a crash left the journal unreadable and the service unable
  to start.
- Participants configured with the same key, or with the operator's key, could
  act as each other.
- Each submission re-netted the whole window; validation is now constant time.

[Unreleased]: https://github.com/SetOff-Org/setoff-clearing/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/SetOff-Org/setoff-clearing/releases/tag/v0.1.0
