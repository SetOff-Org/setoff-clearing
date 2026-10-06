// Package sep10 authenticates participants by their Stellar accounts
// (SEP-10, Stellar Web Authentication).
//
// A participant asks for a challenge transaction, signs it with its account's
// key, and exchanges it for a short-lived bearer token. No long-lived API key
// is needed. A Tessera threshold account signs like any other, since its
// group signature is made under the account's own key.
package sep10

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ChallengeTTL is how long a challenge may be answered.
const ChallengeTTL = 5 * time.Minute

// Errors callers map to responses.
var (
	ErrChallenge = errors.New("invalid challenge")
	ErrToken     = errors.New("invalid or expired token")
)

// Server issues challenges and tokens for one home domain.
type Server struct {
	HomeDomain    string // e.g. clearing.example.org
	WebAuthDomain string // the host serving /auth; usually HomeDomain
	Passphrase    string // network passphrase
	TokenTTL      time.Duration
	Now           func() time.Time

	signer    *keypair.Full
	jwtSecret []byte

	mu   sync.Mutex
	used map[string]time.Time // answered challenge hashes, until they expire
}

// New returns a server signing challenges with seed (an S… secret) and
// tokens with jwtSecret.
func New(seed, homeDomain, passphrase string, jwtSecret []byte) (*Server, error) {
	kp, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	if len(jwtSecret) < 32 {
		return nil, errors.New("the token secret must be at least 32 bytes")
	}
	return &Server{
		HomeDomain: homeDomain, WebAuthDomain: homeDomain, Passphrase: passphrase,
		TokenTTL: time.Hour, Now: time.Now, signer: kp, jwtSecret: jwtSecret, used: map[string]time.Time{},
	}, nil
}

// SigningKey is the G… address challenges are signed with, published as
// SIGNING_KEY in stellar.toml.
func (s *Server) SigningKey() string { return s.signer.Address() }

// Challenge builds a challenge transaction for account, base64 XDR.
func (s *Server) Challenge(account string) (string, error) {
	if _, err := keypair.ParseAddress(account); err != nil || !strings.HasPrefix(account, "G") {
		return "", fmt.Errorf("%w: account must be a G… address", ErrChallenge)
	}
	tx, err := txnbuild.BuildChallengeTx(s.signer.Seed(), account, s.WebAuthDomain, s.HomeDomain, s.Passphrase, ChallengeTTL, nil)
	if err != nil {
		return "", err
	}
	return tx.Base64()
}

// Verify checks a challenge signed by its account and returns a token for
// that account. Each signed challenge is accepted once.
func (s *Server) Verify(signed string) (token, account string, err error) {
	tx, account, _, _, err := txnbuild.ReadChallengeTx(signed, s.SigningKey(), s.Passphrase, s.WebAuthDomain, []string{s.HomeDomain})
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrChallenge, err)
	}
	if _, err := txnbuild.VerifyChallengeTxSigners(signed, s.SigningKey(), s.Passphrase, s.WebAuthDomain, []string{s.HomeDomain}, account); err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrChallenge, err)
	}
	hash, err := tx.HashHex(s.Passphrase)
	if err != nil {
		return "", "", err
	}
	if err := s.spend(hash, time.Unix(tx.Timebounds().MaxTime, 0)); err != nil {
		return "", "", err
	}
	token, err = s.issue(account)
	return token, account, err
}

// spend records a challenge as answered, refusing one answered before.
func (s *Server) spend(hash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	for h, exp := range s.used {
		if now.After(exp) {
			delete(s.used, h)
		}
	}
	if _, seen := s.used[hash]; seen {
		return fmt.Errorf("%w: this challenge was already answered", ErrChallenge)
	}
	s.used[hash] = expires
	return nil
}

type claims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func (s *Server) issue(account string) (string, error) {
	now := s.Now()
	body, err := json.Marshal(claims{Iss: "https://" + s.WebAuthDomain + "/auth", Sub: account, Iat: now.Unix(), Exp: now.Add(s.TokenTTL).Unix()})
	if err != nil {
		return "", err
	}
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + s.mac(unsigned), nil
}

func (s *Server) mac(unsigned string) string {
	m := hmac.New(sha256.New, s.jwtSecret)
	m.Write([]byte(unsigned))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Account returns the account a token was issued to, if it is genuine and current.
func (s *Server) Account(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return "", ErrToken
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(parts[0]+"."+parts[1]))) {
		return "", ErrToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrToken
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil || s.Now().Unix() >= c.Exp {
		return "", ErrToken
	}
	return c.Sub, nil
}

// StellarTOML is the /.well-known/stellar.toml SEP-10 clients discover.
func (s *Server) StellarTOML() string {
	return fmt.Sprintf("NETWORK_PASSPHRASE = %q\nWEB_AUTH_ENDPOINT = %q\nSIGNING_KEY = %q\n",
		s.Passphrase, "https://"+s.WebAuthDomain+"/auth", s.SigningKey())
}

// Passphrase resolves "public", "testnet" or a literal passphrase.
func Passphrase(name string) string {
	switch name {
	case "public", "mainnet":
		return network.PublicNetworkPassphrase
	case "testnet":
		return network.TestNetworkPassphrase
	}
	return name
}
