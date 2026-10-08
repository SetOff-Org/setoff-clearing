// Package settletest is a fake Stellar RPC server for testing settlement.
package settletest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/SetOff-Org/setoff-clearing/internal/settle"
)

// Chain is a fake Stellar RPC server for the settlement contract. Like the
// network, it records authorizations on a first simulation and enforces
// them, signatures and expiry included, once they are attached.
type Chain struct {
	URL string

	t          *testing.T
	passphrase string
	operator   string
	mu         sync.Mutex
	sent       []string // function names, in order
	nonce      int64
	reject     string // simulate this function as failing
}

// Ledger is the chain's latest ledger.
const Ledger = 900_000

// New starts a chain for the operator account on the network with passphrase.
func New(t *testing.T, passphrase, operator string) *Chain {
	c := &Chain{t: t, passphrase: passphrase, operator: operator}
	srv := httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(srv.Close)
	c.URL = srv.URL
	return c
}

// Sent lists the contract functions of the transactions sent, in order.
func (c *Chain) Sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// Reject makes simulations of fn fail with a contract error; "" stops.
func (c *Chain) Reject(fn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reject = fn
}

func (c *Chain) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.t.Error(err)
	}
	var p struct {
		Transaction string `json:"transaction"`
	}
	_ = json.Unmarshal(req.Params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	var result any
	switch req.Method {
	case "getLatestLedger":
		result = map[string]any{"sequence": Ledger}
	case "getLedgerEntries":
		id, _ := xdr.AddressToAccountId(c.operator)
		data, _ := xdr.MarshalBase64(xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{AccountId: id, SeqNum: 41}})
		result = map[string]any{"entries": []map[string]string{{"xdr": data}}}
	case "simulateTransaction":
		result = c.simulate(p.Transaction)
	case "sendTransaction":
		fn, _ := c.call(p.Transaction)
		c.sent = append(c.sent, fn)
		result = map[string]string{"status": "PENDING"}
	case "getTransaction":
		result = map[string]any{"status": "SUCCESS", "ledger": Ledger + 1}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func (c *Chain) call(env string) (string, *xdr.InvokeHostFunctionOp) {
	var e xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(env, &e); err != nil {
		c.t.Fatal(err)
	}
	op := e.Operations()[0].Body.InvokeHostFunctionOp
	return string(op.HostFunction.InvokeContract.FunctionName), op
}

func (c *Chain) simulate(env string) map[string]any {
	fn, op := c.call(env)
	data, _ := xdr.MarshalBase64(xdr.SorobanTransactionData{})
	ok := map[string]any{"transactionData": data, "minResourceFee": "5000", "results": []map[string]any{{"auth": []string{}}}}
	if fn == c.reject {
		return map[string]any{"error": "HostError: Error(Contract, #14)"}
	}
	if fn != "submit" {
		return ok
	}
	if len(op.Auth) > 0 {
		for _, e := range op.Auth {
			a := e.Credentials.Address
			addr, _ := a.Address.String()
			hash, _ := settle.PreimageHash(c.passphrase, e)
			if err := settle.Verify(a.Signature, addr, hash[:]); err != nil {
				return map[string]any{"error": "HostError: Error(Auth, InvalidAction): " + err.Error()}
			}
			if uint32(a.SignatureExpirationLedger) <= Ledger {
				return map[string]any{"error": "HostError: Error(Auth, InvalidAction): expired"}
			}
		}
		return ok
	}
	// Recording mode: one address entry per distinct debtor.
	args := op.HostFunction.InvokeContract
	var auth []string
	seen := map[string]bool{}
	vec, _ := args.Args[0].GetVec()
	for _, o := range *vec {
		m, _ := o.GetMap()
		for _, f := range *m {
			if k, _ := f.Key.GetSym(); k != "debtor" || seen[addrString(f.Val)] {
				continue
			}
			seen[addrString(f.Val)] = true
			c.nonce++
			e := xdr.SorobanAuthorizationEntry{
				Credentials: xdr.SorobanCredentials{
					Type:    xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
					Address: &xdr.SorobanAddressCredentials{Address: *f.Val.Address, Nonce: xdr.Int64(c.nonce), Signature: xdr.ScVal{Type: xdr.ScValTypeScvVoid}},
				},
				RootInvocation: xdr.SorobanAuthorizedInvocation{
					Function: xdr.SorobanAuthorizedFunction{
						Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
						ContractFn: args,
					},
				},
			}
			raw, _ := xdr.MarshalBase64(e)
			auth = append(auth, raw)
		}
	}
	ok["results"] = []map[string]any{{"auth": auth}}
	return ok
}

func addrString(v xdr.ScVal) string {
	s, _ := v.Address.String()
	return s
}
