package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/api"
	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/sep10"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

func TestParticipantsSignInWithTheirStellarAccounts(t *testing.T) {
	c, err := clearing.Open(t.TempDir(), []string{"anchor-a", "anchor-b"})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := sep10.New(keypair.MustRandom().Seed(), "clearing.example.org", network.TestNetworkPassphrase, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	a, stranger := keypair.MustRandom(), keypair.MustRandom()
	s := &api.Server{
		Clearing: c, Participants: map[string]string{}, OperatorKey: "op",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		SEP10: auth, Addresses: map[string]string{a.Address(): "anchor-a"},
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	signIn := func(kp *keypair.Full) (int, map[string]any) {
		_, ch := call(t, srv, "GET", "/auth?account="+kp.Address(), "", nil)
		parsed, err := txnbuild.TransactionFromXDR(ch["transaction"].(string))
		if err != nil {
			t.Fatal(err)
		}
		tx, _ := parsed.Transaction()
		tx, _ = tx.Sign(network.TestNetworkPassphrase, kp)
		signed, _ := tx.Base64()
		return call(t, srv, "POST", "/auth", "", map[string]string{"transaction": signed})
	}

	code, out := signIn(a)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, out)
	}
	token := out["token"].(string)
	if code, out := call(t, srv, "POST", "/v1/obligations", token, owe("anchor-b", "r1", "10")); code != http.StatusCreated {
		t.Fatalf("token not accepted: %d %v", code, out)
	}
	if code, _ := signIn(stranger); code != http.StatusForbidden {
		t.Fatalf("a non-participant got a token: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/auth?account=nope", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad account: %d", code)
	}

	resp, err := http.Get(srv.URL + "/.well-known/stellar.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SIGNING_KEY = \""+auth.SigningKey()+"\"") || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("%s", body)
	}
}
