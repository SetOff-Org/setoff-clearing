package api_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"

	"github.com/SetOff-Org/setoff-clearing/internal/api"
	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/settle"
	"github.com/SetOff-Org/setoff-clearing/internal/settle/settletest"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

const (
	settlementContract = "CCW6QCOSJTTJHDXJOVQ36NVIBAHBUVPMSZNOIJ3A444O6YMVHR4ZEYQV"
	usdcToken          = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

func settlingServer(t *testing.T) (*httptest.Server, *settletest.Chain, map[string]*keypair.Full, chan *settle.Session) {
	t.Helper()
	c, err := clearing.Open(t.TempDir(), []string{"anchor-a", "anchor-b", "anchor-c"})
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]*keypair.Full{"anchor-a": keypair.MustRandom(), "anchor-b": keypair.MustRandom(), "anchor-c": keypair.MustRandom()}
	m := soroban.Mapping{Addresses: map[string]string{}, Tokens: map[string]string{"USDC": usdcToken}}
	for id, kp := range accounts {
		m.Addresses[id] = kp.Address()
	}
	operator := keypair.MustRandom()
	chain := settletest.New(t, network.TestNetworkPassphrase, operator.Address())
	client := settle.NewClient(chain.URL)
	client.Poll = time.Millisecond
	done := make(chan *settle.Session, 8)
	s := &api.Server{
		Clearing: c, Participants: keys, OperatorKey: "op",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: time.Now,
		Settlement: &api.Settlement{
			Settler: &settle.Settler{RPC: client, Passphrase: network.TestNetworkPassphrase, Contract: settlementContract, Operator: operator, Dir: t.TempDir()},
			Mapping: m, Background: context.Background(),
			Done: func(sess *settle.Session, _ error) { done <- sess },
		},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, chain, accounts, done
}

func TestSettlesAWindowThroughTheAPI(t *testing.T) {
	srv, chain, accounts, done := settlingServer(t)
	// a owes c 300, b owes c 200, c owes a 50: a and b are debtors in the plan.
	for key, ob := range map[string]map[string]string{
		"key-a": owe("anchor-c", "inv-1", "300"),
		"key-b": owe("anchor-c", "inv-2", "200"),
		"key-c": owe("anchor-a", "inv-3", "50"),
	} {
		if code, out := call(t, srv, "POST", "/v1/obligations", key, ob); code != http.StatusCreated {
			t.Fatalf("submit: %d %v", code, out)
		}
	}
	if code, out := call(t, srv, "POST", "/v1/window/close", "op", nil); code != 200 {
		t.Fatalf("close: %d %v", code, out)
	}

	if code, _ := call(t, srv, "POST", "/v1/windows/1/settlement", "key-a", nil); code != http.StatusUnauthorized {
		t.Fatalf("a participant prepared settlement: %d", code)
	}
	if code, out := call(t, srv, "GET", "/v1/windows/1/authorizations", "key-a", nil); code != http.StatusNotFound {
		t.Fatalf("before prepare: %d %v", code, out)
	}
	if code, out := call(t, srv, "POST", "/v1/windows/1/settlement", "op", nil); code != 200 {
		t.Fatalf("prepare: %d %v", code, out)
	}

	sign := func(key, id string) []map[string]any {
		_, out := call(t, srv, "GET", "/v1/windows/1/authorizations", key, nil)
		var sigs []map[string]any
		for _, a := range out["authorizations"].([]any) {
			a := a.(map[string]any)
			hash, _ := hex.DecodeString(a["hash"].(string))
			sig, _ := accounts[id].Sign(hash)
			sigs = append(sigs, map[string]any{"batch": a["batch"], "slot": a["slot"], "signature": hex.EncodeToString(sig)})
		}
		return sigs
	}
	aSigs, bSigs := sign("key-a", "anchor-a"), sign("key-b", "anchor-b")
	if len(aSigs) != 1 || len(bSigs) != 1 {
		t.Fatalf("each debtor signs once: a %v, b %v", aSigs, bSigs)
	}
	if _, out := call(t, srv, "GET", "/v1/windows/1/authorizations", "key-c", nil); len(out["authorizations"].([]any)) != 0 {
		t.Fatalf("c owes nothing but was asked to sign: %v", out)
	}

	// c can't sign for a, and a's signature can't fill b's slot.
	if code, out := call(t, srv, "POST", "/v1/windows/1/authorizations", "key-c", map[string]any{"authorizations": aSigs}); code != http.StatusUnprocessableEntity {
		t.Fatalf("c signed for a: %d %v", code, out)
	}
	swapped := map[string]any{"batch": 0, "slot": bSigs[0]["slot"], "signature": aSigs[0]["signature"]}
	if code, _ := call(t, srv, "POST", "/v1/windows/1/authorizations", "key-b", map[string]any{"authorizations": []any{swapped}}); code != http.StatusUnprocessableEntity {
		t.Fatal("a's signature filled b's slot")
	}

	code, out := call(t, srv, "POST", "/v1/windows/1/authorizations", "key-a", map[string]any{"authorizations": aSigs})
	if code != http.StatusAccepted || out["pending"].(float64) != 1 {
		t.Fatalf("a: %d %v", code, out)
	}
	<-done // nothing to submit yet
	if len(chain.Sent()) != 0 {
		t.Fatalf("submitted before b signed: %v", chain.Sent())
	}
	if code, _ := call(t, srv, "POST", "/v1/windows/1/authorizations", "key-b", map[string]any{"authorizations": bSigs}); code != http.StatusAccepted {
		t.Fatal("b")
	}
	select {
	case sess := <-done:
		if !sess.Settled() {
			t.Fatalf("not settled: %+v", sess.Settle)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("settlement did not finish")
	}
	if got := strings.Join(chain.Sent(), ","); got != "submit,settle" {
		t.Fatalf("sent %s", got)
	}
	code, out = call(t, srv, "GET", "/v1/windows/1/settlement", "key-c", nil)
	if code != 200 || out["settled"] != true || out["pending"].(float64) != 0 {
		t.Fatalf("status: %d %v", code, out)
	}
}

func TestSettlementRoutesWithoutSettlement(t *testing.T) {
	srv := server(t, t.TempDir())
	for _, r := range [][3]string{
		{"POST", "/v1/windows/1/settlement", "op"},
		{"GET", "/v1/windows/1/settlement", "op"},
		{"GET", "/v1/windows/1/authorizations", "key-a"},
		{"POST", "/v1/windows/1/authorizations", "key-a"},
	} {
		if code, out := call(t, srv, r[0], r[1], r[2], map[string]any{}); code != http.StatusNotFound || !strings.Contains(out["error"].(string), "not configured") {
			t.Errorf("%s %s: %d %v", r[0], r[1], code, out)
		}
	}
}
