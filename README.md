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
  open window, anyone in the group can preview the netting, and the operator
  closes the window, which nets it, archives it, and opens the next one.

The netting algorithm is an independent Go implementation of
[setoff-engine](https://github.com/SetOff-Org/setoff-engine). The tests require
it to reproduce every Rust reference vector exactly, including a
200-obligation window, and CI checks the vectors are current.

## Clearing service

```toml
# setoff.toml
listen = "127.0.0.1:7500"
data_dir = "data"
operator_key_env = "SETOFF_OPERATOR_KEY"

[[participant]]
id = "anchor-ng"
key_env = "SETOFF_KEY_ANCHOR_NG"

[[participant]]
id = "anchor-us"
key_env = "SETOFF_KEY_ANCHOR_US"
```

| Route | Who | What |
|---|---|---|
| `POST /v1/obligations` | participant | `{"reference", "creditor", "asset", "amount"}`. The debtor is always the caller: you can only commit yourself to pay |
| `GET /v1/window` | any | Open window: obligation count and live netting |
| `POST /v1/window/close` | operator | Net, archive and open the next window |
| `GET /v1/windows/{n}` | any | An archived window |

Every obligation is journaled with `fsync` before it is acknowledged, so a
restart loses nothing, and duplicate references are rejected even across
restarts.

## Roadmap

1. Submit closed windows to
   [setoff-contracts](https://github.com/SetOff-Org/setoff-contracts), with
   participant-signed Soroban authorization entries.
2. SEP-10 authentication for participants instead of API keys.
3. Settlement reports (ISO 20022 `camt.053`) for treasury systems.

## License

[Apache-2.0](LICENSE)
