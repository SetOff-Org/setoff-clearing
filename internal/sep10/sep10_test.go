package sep10

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func server(t *testing.T) *Server {
	t.Helper()
	s, err := New(keypair.MustRandom().Seed(), "clearing.example.org", network.TestNetworkPassphrase, secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// sign answers a challenge as the client would.
func sign(t *testing.T, challenge string, kp *keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(challenge)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := parsed.Transaction()
	tx, err = tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tx.Base64()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnAccountSignsInAndGetsAToken(t *testing.T) {
	s := server(t)
	client := keypair.MustRandom()
	challenge, err := s.Challenge(client.Address())
	if err != nil {
		t.Fatal(err)
	}
	token, account, err := s.Verify(sign(t, challenge, client))
	if err != nil || account != client.Address() {
		t.Fatalf("%q %v", account, err)
	}
	if got, err := s.Account(token); err != nil || got != client.Address() {
		t.Fatalf("token resolves to %q, %v", got, err)
	}
	if !strings.Contains(s.StellarTOML(), `SIGNING_KEY = "`+s.SigningKey()+`"`) {
		t.Fatal(s.StellarTOML())
	}
}

func TestOnlyTheAccountCanAnswer(t *testing.T) {
	s := server(t)
	client, other := keypair.MustRandom(), keypair.MustRandom()
	challenge, _ := s.Challenge(client.Address())
	if _, _, err := s.Verify(sign(t, challenge, other)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("someone else's signature: %v", err)
	}
	if _, _, err := s.Verify(challenge); !errors.Is(err, ErrChallenge) {
		t.Fatalf("unsigned: %v", err)
	}
	// A challenge from another server is not ours to accept.
	foreign, _ := server(t).Challenge(client.Address())
	if _, _, err := s.Verify(sign(t, foreign, client)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("foreign challenge: %v", err)
	}
	if _, err := s.Challenge("not-an-account"); !errors.Is(err, ErrChallenge) {
		t.Fatalf("bad account: %v", err)
	}
}

func TestASignedChallengeWorksOnce(t *testing.T) {
	s := server(t)
	client := keypair.MustRandom()
	challenge, _ := s.Challenge(client.Address())
	signed := sign(t, challenge, client)
	if _, _, err := s.Verify(signed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Verify(signed); !errors.Is(err, ErrChallenge) {
		t.Fatalf("replayed: %v", err)
	}
}

func TestTokensExpireAndCannotBeForged(t *testing.T) {
	s := server(t)
	client := keypair.MustRandom()
	challenge, _ := s.Challenge(client.Address())
	token, _, err := s.Verify(sign(t, challenge, client))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	forged := parts[0] + "." + parts[1] + "x." + parts[2]
	for name, tok := range map[string]string{"forged": forged, "garbage": "a.b", "empty": ""} {
		if _, err := s.Account(tok); !errors.Is(err, ErrToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other, _ := New(keypair.MustRandom().Seed(), "clearing.example.org", network.TestNetworkPassphrase, []byte("another-secret-of-thirty-two-bytes!"))
	if _, err := other.Account(token); !errors.Is(err, ErrToken) {
		t.Fatalf("another server's secret: %v", err)
	}
	s.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.Account(token); !errors.Is(err, ErrToken) {
		t.Fatalf("expired: %v", err)
	}
}

func TestShortSecretsAreRefused(t *testing.T) {
	if _, err := New(keypair.MustRandom().Seed(), "x.org", network.TestNetworkPassphrase, []byte("short")); err == nil {
		t.Fatal("a short token secret must be refused")
	}
}
