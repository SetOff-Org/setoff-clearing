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

The netting algorithm is an independent Go implementation of
[setoff-engine](https://github.com/SetOff-Org/setoff-engine). The tests require
it to reproduce every Rust reference vector exactly, including a
200-obligation window, and CI checks the vectors are current.

## Clearing service

```sh
cp examples/setoff.toml .   # participants, keys, assets, schedule
setoff serve
```

[`examples/setoff.toml`](examples/setoff.toml) documents every setting. Each
participant has its own key; the service refuses to start if two parties share
a key, so nobody can act as anyone else.

| Route | Who | What |
|---|---|---|
| `POST /v1/obligations` | participant | `{"reference", "creditor", "asset", "amount"}`. The debtor is always the caller: you can only commit yourself to pay |
| `GET /v1/me` | participant | Your obligations, net positions and plan legs in the open window |
| `GET /v1/window` | any | Open window: obligation count and live netting |
| `POST /v1/window/close` | operator | Net, archive and open the next window |
| `GET /v1/windows` | any | Archived windows, newest first |
| `GET /v1/windows/{n}` | any | An archived window |
| `GET /v1/windows/{n}/camt053` | any | camt.053 statements: your own, or all of them for the operator |
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
stellar contract invoke --id CCV7S3…WNGU --source operator --network testnet -- submit --obligations '[{"debtor":"GAIH3U…","creditor":"GBRPYH…","token":"CBIELT…","amount":"1450000000","reference":"5f2c…"}]'
stellar contract invoke --id CCV7S3…WNGU --source operator --network testnet -- settle
```

Each leg's reference is derived from the window and leg number, so a batch
resubmitted after a timeout is rejected by the contract rather than booked
twice. Every submit must be authorized by its debtors; a debtor that is a
[Tessera](https://github.com/Use-Tessera) threshold account signs through its
coordinator's `/v1/authorize`.

## Treasury reports

```sh
setoff report --window 7 --participant anchor-ng > window-7.camt053.xml
```

One statement per participant and asset: each obligation is a booked entry and
the net position is the closing balance. `[decimals]` turns smallest units into
whole units; assets without an ISO 4217 code are reported under `XXX` and named
in the statement.

## Roadmap

1. SEP-10 authentication for participants instead of API keys.
2. Submitting settlement batches directly, collecting debtors' authorization
   entries over the API.

## License

[Apache-2.0](LICENSE)
