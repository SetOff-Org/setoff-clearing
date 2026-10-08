package settle

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

const (
	contract = "CCW6QCOSJTTJHDXJOVQ36NVIBAHBUVPMSZNOIJ3A444O6YMVHR4ZEYQV"
	token    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

func TestObligationEncoding(t *testing.T) {
	v, err := obligationsVal([]soroban.Obligation{{Debtor: keypair.MustRandom().Address(), Creditor: contract, Token: token, Amount: "18446744073709551621", Reference: soroban.Reference(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	vec, _ := v.GetVec()
	m, _ := (*vec)[0].GetMap()
	var keys []string
	for _, e := range *m {
		k, _ := e.Key.GetSym()
		keys = append(keys, string(k))
	}
	if strings.Join(keys, ",") != "amount,creditor,debtor,reference,token" {
		t.Fatalf("fields must be sorted: %v", keys)
	}
	if i := (*m)[0].Val.I128; i.Hi != 1 || i.Lo != 5 {
		t.Fatalf("2^64+5 encoded as %+v", i)
	}
	for _, bad := range []soroban.Obligation{
		{Debtor: "nope", Creditor: contract, Token: token, Amount: "1", Reference: soroban.Reference(1, 0)},
		{Debtor: contract, Creditor: contract, Token: token, Amount: "0", Reference: soroban.Reference(1, 0)},
		{Debtor: contract, Creditor: contract, Token: token, Amount: "1", Reference: "abc"},
	} {
		if _, err := obligationsVal([]soroban.Obligation{bad}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
