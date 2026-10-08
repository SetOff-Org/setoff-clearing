#!/usr/bin/env bash
# On-chain settlement through the clearing service, end to end on testnet:
# deploys a fresh settlement contract, records obligations over the API,
# closes the window, has each debtor sign its authorization with
# `setoff authorize`, and lets the service submit and settle.
#
# Needs stellar-cli, curl, Go, and a checkout of SetOff-Org/setoff-contracts
# (SETOFF_CONTRACTS, default ../setoff-contracts). Uses port 7599.
set -euo pipefail
cd "$(dirname "$0")/.."
CONTRACTS=${SETOFF_CONTRACTS:-../setoff-contracts}
NETWORK=testnet
WORK=$(mktemp -d)
PID=
trap '[ -n "$PID" ] && kill "$PID" 2>/dev/null; rm -rf "$WORK"' EXIT

go build -o "$WORK/setoff" ./cmd/setoff
(cd "$CONTRACTS" && stellar contract build >/dev/null 2>&1)
WASM="$CONTRACTS/target/wasm32v1-none/release/setoff_settlement.wasm"

echo "==> funding an operator and three participants"
for who in clr-operator clr-a clr-b clr-c; do
  stellar keys generate "$who" --network "$NETWORK" --fund --overwrite >/dev/null 2>&1
done
OP=$(stellar keys address clr-operator)
A=$(stellar keys address clr-a)
B=$(stellar keys address clr-b)
C=$(stellar keys address clr-c)
XLM=$(stellar contract id asset --asset native --network "$NETWORK")

invoke() { stellar contract invoke --id "$ID" --source "$1" --network "$NETWORK" --send yes -- "${@:2}" >/dev/null 2>&1; }
ID=$(stellar contract deploy --wasm "$WASM" --source clr-operator --network "$NETWORK" -- --admin "$OP" 2>/dev/null)
echo "    contract $ID"
invoke clr-operator set_token --token "$XLM" --allowed true
invoke clr-operator admit_many --members "[\"$A\",\"$B\",\"$C\"]"
invoke clr-a deposit --member "$A" --token "$XLM" --amount 50000000
invoke clr-b deposit --member "$B" --token "$XLM" --amount 50000000
echo "    A and B each deposit 5 XLM collateral"

cat >"$WORK/setoff.toml" <<EOF
listen = "127.0.0.1:7599"
data_dir = "data"
operator_key_env = "SETOFF_OPERATOR_KEY"
contract = "$ID"

[tokens]
XLM = "$XLM"

[settlement]
rpc = "https://soroban-testnet.stellar.org"
network = "testnet"
operator_secret_env = "SETOFF_OPERATOR_SECRET"

[[participant]]
id = "a"
key_env = "KEY_A"
address = "$A"

[[participant]]
id = "b"
key_env = "KEY_B"
address = "$B"

[[participant]]
id = "c"
key_env = "KEY_C"
address = "$C"
EOF
export SETOFF_OPERATOR_KEY="op-$RANDOM$RANDOM" KEY_A="a-$RANDOM$RANDOM" KEY_B="b-$RANDOM$RANDOM" KEY_C="c-$RANDOM$RANDOM"
SETOFF_OPERATOR_SECRET=$(stellar keys show clr-operator) "$WORK/setoff" serve --config "$WORK/setoff.toml" 2>"$WORK/serve.log" &
PID=$!
URL=http://127.0.0.1:7599
for _ in $(seq 50); do curl -fs "$URL/healthz" >/dev/null && break; sleep 0.2; done

api() { curl -fsS -X "$1" -H "Authorization: Bearer $2" -H 'Content-Type: application/json' "$URL$3" ${4:+-d "$4"}; }
owe() { printf '{"reference":"%s","creditor":"%s","asset":"XLM","amount":"%s"}' "$1" "$2" "$3"; }

echo "==> recording obligations: A owes C 3 XLM, B owes C 2 XLM, C owes A 1 XLM"
api POST "$KEY_A" /v1/obligations "$(owe inv-1 c 30000000)" >/dev/null
api POST "$KEY_B" /v1/obligations "$(owe inv-2 c 20000000)" >/dev/null
api POST "$KEY_C" /v1/obligations "$(owe inv-3 a 10000000)" >/dev/null
api POST "$SETOFF_OPERATOR_KEY" /v1/window/close >/dev/null
echo "    window 1 closed: 6 XLM gross nets to A pays C 2, B pays C 2"

echo "==> preparing settlement"
api POST "$SETOFF_OPERATOR_KEY" /v1/windows/1/settlement >/dev/null

echo "==> each debtor signs its own authorization"
SECRET_A=$(stellar keys show clr-a) SECRET_B=$(stellar keys show clr-b)
export SECRET_A SECRET_B
"$WORK/setoff" authorize --url "$URL" --window 1 --key-env KEY_A --secret-env SECRET_A | sed 's/^/    A: /'
"$WORK/setoff" authorize --url "$URL" --window 1 --key-env KEY_B --secret-env SECRET_B | sed 's/^/    B: /'

echo "==> waiting for the service to submit and settle"
status=
for _ in $(seq 60); do
  status=$(api GET "$SETOFF_OPERATOR_KEY" /v1/windows/1/settlement)
  grep -q '"settled":true' <<<"$status" && break
  sleep 2
done
if ! grep -q '"settled":true' <<<"$status"; then
  echo "not settled:" >&2
  echo "$status" >&2
  cat "$WORK/serve.log" >&2
  exit 1
fi
# Transaction hashes only, not the authorization entries' preimage hashes.
grep -oE '"(tx|settle)":\{"hash":"[0-9a-f]{64}"' <<<"$status" | sed -E 's/"(tx|settle)":\{"hash":"([0-9a-f]+)"/\1 \2/; s/^tx /    submit: /; s/^settle /    settle: /'
