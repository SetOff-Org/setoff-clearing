package settle_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
	"github.com/SetOff-Org/setoff-clearing/internal/settle"
	"github.com/SetOff-Org/setoff-clearing/internal/settle/settletest"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

const (
	contract = "CCW6QCOSJTTJHDXJOVQ36NVIBAHBUVPMSZNOIJ3A444O6YMVHR4ZEYQV"
	token    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

type party struct {
	id string
	kp *keypair.Full
}

func setup(t *testing.T) (*settle.Settler, *settletest.Chain, *clearing.Closed, soroban.Mapping, []party) {
	t.Helper()
	parties := []party{{"a", keypair.MustRandom()}, {"b", keypair.MustRandom()}, {"c", keypair.MustRandom()}}
	s := &settle.Settler{Passphrase: network.TestNetworkPassphrase, Contract: contract, Operator: keypair.MustRandom(), Dir: t.TempDir()}
	c := settletest.New(t, s.Passphrase, s.Operator.Address())
	client := settle.NewClient(c.URL)
	client.Poll = time.Millisecond
	s.RPC = client

	m := soroban.Mapping{Addresses: map[string]string{}, Tokens: map[string]string{"XLM": token}}
	for _, p := range parties {
		m.Addresses[p.id] = p.kp.Address()
	}
	// a owes b 10, b owes c 4: the plan has two legs, a and b are debtors.
	w := &clearing.Closed{Window: 7, Netting: &netting.Result{Transfers: []netting.Transfer{
		{From: "a", To: "b", Asset: "XLM", Amount: netting.NewAmount(6)},
		{From: "a", To: "c", Asset: "XLM", Amount: netting.NewAmount(4)},
		{From: "b", To: "c", Asset: "XLM", Amount: netting.NewAmount(0x7fffffffffffffff)},
	}}}
	return s, c, w, m, parties
}

// sign answers every entry for p with a bare signature of its hash.
func sign(t *testing.T, sess *settle.Session, p party) []settle.Signature {
	t.Helper()
	var out []settle.Signature
	for i, b := range sess.Batches {
		for _, e := range b.Entries {
			if e.Address != p.kp.Address() {
				continue
			}
			hash, _ := hex.DecodeString(e.Hash)
			sig, err := p.kp.Sign(hash)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, settle.Signature{Batch: i, Slot: e.Slot, Signature: hex.EncodeToString(sig)})
		}
	}
	return out
}

func TestSettlesAClosedWindow(t *testing.T) {
	s, c, w, m, parties := setup(t)
	ctx := context.Background()
	sess, err := s.Prepare(ctx, w, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Batches) != 1 || len(sess.Batches[0].Entries) != 2 || sess.Pending() != 2 {
		t.Fatalf("want one batch with entries for debtors a and b, got %+v", sess.Batches)
	}
	if e := sess.Batches[0].Entries[0]; e.ExpirationLedger != settletest.Ledger+settle.DefaultValidity {
		t.Fatalf("expiry %d", e.ExpirationLedger)
	}

	// Nothing is sent until every debtor has signed.
	if _, err := s.Accept(7, parties[0].kp.Address(), sign(t, sess, parties[0])); err != nil {
		t.Fatal(err)
	}
	if sess, err = s.Advance(ctx, 7); err != nil || len(c.Sent()) != 0 || sess.Pending() != 1 {
		t.Fatalf("advanced with a signature missing: %v %v", err, c.Sent())
	}

	// b signs with a whole entry, the way SDKs return it.
	e := sess.Batches[0].Entries[1]
	var entry xdr.SorobanAuthorizationEntry
	_ = xdr.SafeUnmarshalBase64(e.Entry, &entry)
	hash, _ := hex.DecodeString(e.Hash)
	sig, _ := parties[1].kp.Sign(hash)
	signed, _ := xdr.MarshalBase64(settle.WithSignature(entry, parties[1].kp.Address(), sig))
	if _, err := s.Accept(7, parties[1].kp.Address(), []settle.Signature{{Batch: 0, Slot: e.Slot, Entry: signed}}); err != nil {
		t.Fatal(err)
	}

	sess, err = s.Advance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Settled() || strings.Join(c.Sent(), ",") != "submit,settle" {
		t.Fatalf("want submit then settle, sent %v, session %+v", c.Sent(), sess.Settle)
	}
	// Advancing a settled window sends nothing more.
	if _, err := s.Advance(ctx, 7); err != nil || len(c.Sent()) != 2 {
		t.Fatalf("resent: %v %v", err, c.Sent())
	}
}

func TestRefusesBadAuthorizations(t *testing.T) {
	s, _, w, m, parties := setup(t)
	sess, err := s.Prepare(context.Background(), w, m)
	if err != nil {
		t.Fatal(err)
	}
	a, b := parties[0], parties[1]
	mine := sign(t, sess, a)[0]
	e := sess.Batches[0].Entries[0]
	var entry xdr.SorobanAuthorizationEntry
	_ = xdr.SafeUnmarshalBase64(e.Entry, &entry)
	hash, _ := hex.DecodeString(e.Hash)
	sig, _ := a.kp.Sign(hash)

	// A signed entry whose expiry was stretched no longer matches what was issued.
	stretched := settle.WithSignature(entry, a.kp.Address(), sig)
	stretched.Credentials.Address.SignatureExpirationLedger += 100_000
	longer, _ := xdr.MarshalBase64(stretched)

	cases := map[string]struct {
		who string
		sig settle.Signature
	}{
		"someone else's slot": {b.kp.Address(), mine},
		"wrong key":           {a.kp.Address(), sign(t, sess, b)[0]},
		"tampered entry":      {a.kp.Address(), settle.Signature{Batch: 0, Slot: e.Slot, Entry: longer}},
		"no such batch":       {a.kp.Address(), settle.Signature{Batch: 3, Slot: e.Slot, Signature: mine.Signature}},
		"nothing to check":    {a.kp.Address(), settle.Signature{Batch: 0, Slot: e.Slot}},
		"short signature":     {a.kp.Address(), settle.Signature{Batch: 0, Slot: e.Slot, Signature: "abcd"}},
		"entry and signature": {a.kp.Address(), settle.Signature{Batch: 0, Slot: e.Slot, Entry: longer, Signature: mine.Signature}},
	}
	for name, tc := range cases {
		if name == "wrong key" {
			tc.sig.Slot = e.Slot // b's signature, offered for a's slot
		}
		if _, err := s.Accept(7, tc.who, []settle.Signature{tc.sig}); !errors.Is(err, settle.ErrInvalid) {
			t.Errorf("%s: want settle.ErrInvalid, got %v", name, err)
		}
	}
	// One bad signature in a set rejects the whole set.
	if _, err := s.Accept(7, a.kp.Address(), []settle.Signature{mine, {Batch: 0, Slot: e.Slot, Signature: "abcd"}}); !errors.Is(err, settle.ErrInvalid) {
		t.Fatal(err)
	}
	if got, _ := s.Session(7); got.Pending() != 2 {
		t.Fatalf("a rejected set was partly recorded: %d pending", got.Pending())
	}
	if _, err := s.Accept(8, a.kp.Address(), []settle.Signature{mine}); !errors.Is(err, settle.ErrNotPrepared) {
		t.Fatal(err)
	}
}

func TestRecordsARejectedSubmitAndRetries(t *testing.T) {
	s, c, w, m, parties := setup(t)
	ctx := context.Background()
	sess, _ := s.Prepare(ctx, w, m)
	for _, p := range parties[:2] {
		if _, err := s.Accept(7, p.kp.Address(), sign(t, sess, p)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reject("settle")
	sess, err := s.Advance(ctx, 7)
	if err == nil || sess.Settle.Status != settle.StatusRejected || !strings.Contains(sess.Settle.Error, "#14") {
		t.Fatalf("want a recorded rejection, got %v %+v", err, sess.Settle)
	}
	if sess.Batches[0].Tx.Status != "SUCCESS" {
		t.Fatal("the submit should have gone through")
	}
	// After the cause is fixed, Advance settles without resubmitting the batch.
	c.Reject("")
	if sess, err = s.Advance(ctx, 7); err != nil || !sess.Settled() || strings.Join(c.Sent(), ",") != "submit,settle" {
		t.Fatalf("%v %v", err, c.Sent())
	}
	// Preparing again keeps the settled batch.
	again, err := s.Prepare(ctx, w, m)
	if err != nil || again.Batches[0].Tx == nil || again.Batches[0].Tx.Status != "SUCCESS" {
		t.Fatalf("re-prepare dropped the settled batch: %v", err)
	}
}
