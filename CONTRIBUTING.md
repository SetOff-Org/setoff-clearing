# Contributing

The clearing service records obligations, nets windows and settles them on
chain. It takes part in the [Stellar Wave](https://www.drips.network/wave/stellar)
program; Wave issues are labeled with their complexity. The ground rules for
every SetOff repository are in the
[organization guide](https://github.com/SetOff-Org/.github/blob/main/CONTRIBUTING.md).

## Setup

```sh
git clone https://github.com/SetOff-Org/setoff-clearing && cd setoff-clearing
go test ./...
```

Go 1.25 or later. Tests run offline; on-chain settlement is tested against
`internal/settle/settletest`, a fake RPC server that records and then enforces
Soroban authorizations the way the network does.

## Before you open a PR

```sh
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
golangci-lint run
```

CI also runs govulncheck, builds the Docker image, and checks the reference
vectors.

## Rules specific to the service

- **The engines must agree.** `internal/netting` is a Go port of
  [setoff-engine](https://github.com/SetOff-Org/setoff-engine), and
  `testdata/vectors` must be identical to its `tests/vectors`; CI compares them.
  Netting changes land in the engine first.
- **A new endpoint needs an entry in `api/openapi.yaml`**; a test checks that
  every route is described.
- **Journals are the source of truth.** They are append-only, and recovery
  after a torn write is tested; a change to the journal format keeps that test
  passing.
- **Settlement never trusts what it is handed.** A signed authorization is
  accepted only if it is the issued entry plus a valid signature. Keep it that
  way, and test the refusal paths.
- **Try it on testnet.** [`scripts/testnet-settle.sh`](scripts/testnet-settle.sh)
  deploys a contract and settles a window through the service.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org): `feat(api): …`,
`fix(settle): …`, `docs: …`.
