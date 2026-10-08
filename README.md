<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo.svg" alt="SetOff" height="64">
  </picture>
</p>

<p align="center"><b>The SetOff clearing service and CLI. Collect obligations in windows, see who really owes whom, and settle the difference.</b></p>

<p align="center">
  <a href="https://github.com/SetOff-Org/setoff-clearing/actions/workflows/ci.yml"><img src="https://github.com/SetOff-Org/setoff-clearing/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License"></a>
</p>

---

```console
$ setoff net corridor.csv
ASSET  OBLIGATIONS  GROSS        SETTLED     SAVED   TRANSFERS
EURC   2            1350000000   50000000    96.29%  1
USDC   5            11450000000  2900000000  74.67%  2

PLAN
EURC  anchor-eu → anchor-ng  50000000
USDC  anchor-ng → anchor-ke  1450000000
USDC  anchor-ng → anchor-us  1450000000
```

Seven obligations between four anchors (1,145 USDC and 135 EURC gross) settle with three
transfers. This repository holds:

- **`setoff net`**: nets a CSV or JSON file of obligations and prints positions,
  the plan and the liquidity saved (`--json` for the full result).
- **`setoff serve`**: a clearing service. Participants post obligations into the
  open window and see their side of it; the operator, or a schedule, closes the
  window, which nets it, archives it, and opens the next one.
- **`setoff report`**: ISO 20022 camt.053 statements for a closed window.
- **`setoff soroban`**: the calls that settle a closed window through the
  [settlement contract](https://github.com/SetOff-Org/setoff-contracts).
  With `[settlement]` configured, `setoff serve` does it itself.
- **`setoff authorize`**: a debtor's side of that, signing its authorizations
  with its own account key.

The netting algorithm is an independent Go implementation of
[setoff-engine](https://github.com/SetOff-Org/setoff-engine). The tests require
it to reproduce every Rust reference vector exactly, including a
200-obligation window, and CI checks the vectors are current.

## Install

```sh
go install github.com/SetOff-Org/setoff-clearing/cmd/setoff@latest
docker run --rm ghcr.io/setoff-org/setoff version   # amd64 and arm64
```

Binaries for Linux, macOS and Windows are on
[Releases](https://github.com/SetOff-Org/setoff-clearing/releases).

## Clearing service

```sh
cp examples/setoff.toml .   # participants, keys, assets, schedule
setoff serve
```

[`examples/setoff.toml`](examples/setoff.toml) documents every setting. Each
participant has its own key; the service refuses to start if two parties share
a key, so nobody can act as anyone else.

**Signing in with a Stellar account.** With `[sep10]` configured, a participant
can skip API keys: `GET /auth?account=G…` returns a challenge, the participant
signs it with its account key, and `POST /auth` returns a bearer token valid
for an hour (SEP-10, with the signing key published at
`/.well-known/stellar.toml`). A participant controlled by a
[Tessera](https://github.com/Use-Tessera) threshold group signs in the same way.

| Route | Who | What |
|---|---|---|
| `POST /v1/obligations` | participant | `{"reference", "creditor", "asset", "amount"}`. The debtor is always the caller: you can only commit yourself to pay |
| `GET /v1/me` | participant | Your obligations, net positions and plan legs in the open window |
| `GET /v1/window` | any | Open window: obligation count and live netting |
| `POST /v1/window/close` | operator | Net, archive and open the next window |
| `GET /v1/windows` | any | Archived windows, newest first |
| `GET /v1/windows/{n}` | any | An archived window |
| `GET /v1/windows/{n}/camt053` | any | camt.053 statements: your own, or all of them for the operator |
| `GET /auth`, `POST /auth` | open | SEP-10 challenge and token, with `[sep10]` |
| `GET /metrics` | open | Prometheus counters by route |

The full schema is in [`api/openapi.yaml`](api/openapi.yaml).

**Durability.** Every obligation is journaled with `fsync` before it is
acknowledged. A crash mid-write drops only the unacknowledged line, and a
crash while closing can never replay an archived window into the next one.
References are unique per debtor and window, across restarts: an identical
retry gets the original id back (`"replayed": true`), anything else under a
used reference is a 409.

**Scheduling and notifications.** `close_every = "1h"` closes non-empty windows
on a schedule. With `[webhook]` set, every close is POSTed as JSON, signed with
`X-SetOff-Signature: sha256=HMAC(secret, "<X-SetOff-Timestamp>.<body>")` and
retried with backoff.

## Settling on chain

The settlement contract nets on chain too, but it does not need every gross
obligation. `setoff soroban` submits the window's netted plan: the same net
positions in at most one leg fewer than participants per asset.

```console
$ setoff soroban --window 7 --network testnet
# window 7: 1 submit call(s), then settle
stellar contract invoke --id CCW6QC…EYQV --source operator --network testnet -- submit --obligations '[{"debtor":"GAIH3U…","creditor":"GBRPYH…","token":"CBIELT…","amount":"1450000000","reference":"5f2c…"}]'
stellar contract invoke --id CCW6QC…EYQV --source operator --network testnet -- settle
```

Each leg's reference is derived from the window and leg number, so a batch
resubmitted after a timeout is rejected by the contract rather than booked
twice.

### Letting the service settle

Every submit must be authorized by its debtors, so the service collects their
authorizations over the API and submits the batches itself. Add:

```toml
[settlement]
rpc = "https://soroban-testnet.stellar.org"
network = "testnet"
operator_secret_env = "SETOFF_OPERATOR_SECRET"  # the contract's admin
prepare_on_close = true                         # or POST /v1/windows/{n}/settlement
```

1. When a window closes, the service simulates each `submit` batch and records
   the Soroban authorization entries the contract asks of each debtor, valid for
   `validity_ledgers` (default 720, about an hour).
2. Each debtor fetches its own from `GET /v1/windows/{n}/authorizations` and
   posts them back signed: the whole entry, as Stellar SDKs' `authorizeEntry`
   return it, or for an account just the Ed25519 signature of its `hash`.
   `setoff authorize` does this with a key from the environment. A debtor that is
   a [Tessera](https://github.com/Use-Tessera) threshold account signs the entry
   through its coordinator's `/v1/authorize`.
3. The service accepts a signed entry only if it is the issued one plus a valid
   signature, byte for byte. Once a batch is fully signed it simulates, submits
   and waits for it, then calls `settle` after the last one.
   `GET /v1/windows/{n}/settlement` shows progress; failures are recorded there
   and retried on the next signature or re-prepare.

Sessions are kept in `data_dir/settlement/`, so a restart resumes where it left
off. [`scripts/testnet-settle.sh`](scripts/testnet-settle.sh) runs the whole
flow on testnet against a freshly deployed contract:

| What | Transaction |
|---|---|
| Two debtors' authorizations, one `submit` of the netted plan | [`79b3d9d6…e9d4`](https://stellar.expert/explorer/testnet/tx/79b3d9d6058339d3b82f9c0908a52dfdb3a9ecbbe6c23e163c0bb6431eb2e9d4) |
| `settle` | [`33409ebd…2c83`](https://stellar.expert/explorer/testnet/tx/33409ebdbb2e48f9b632c01476f18930a13dff1c864f032efc95d7dad4c92c83) |

## Treasury reports

```sh
setoff report --window 7 --participant anchor-ng > window-7.camt053.xml
```

One statement per participant and asset: each obligation is a booked entry and
the net position is the closing balance. `[decimals]` turns smallest units into
whole units; assets without an ISO 4217 code are reported under `XXX` and named
in the statement.

## Roadmap

1. Collecting authorizations from contract-account debtors with custom
   `__check_auth`, which today are checked only at simulation.

## License

[Apache-2.0](LICENSE)
