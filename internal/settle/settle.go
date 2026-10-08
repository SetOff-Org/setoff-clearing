// Package settle settles closed windows through the SetOff settlement
// contract, collecting each debtor's Soroban authorization over the API.
//
// The contract's submit requires every debtor in a batch to authorize it.
// Prepare simulates each batch to learn exactly which authorization entries
// the contract will ask for, and keeps them unsigned. Each debtor fetches its
// own, signs, and hands them back; Accept checks that a signed entry is the
// unsigned one plus a valid signature, nothing else. Advance then submits
// every batch whose entries are all signed, and settle once all are in.
//
// Sessions are kept as JSON files, one per window, so a restart picks up
// where it left off. Each leg's on-chain reference is derived from its window
// and position, so a batch resubmitted after a timeout is refused by the
// contract rather than booked twice.
package settle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

// DefaultValidity is how many ledgers (about 5 s each) debtors have to sign.
const DefaultValidity = 720

// Transaction statuses besides the network's SUCCESS and FAILED.
const (
	// StatusRejected means simulation refused the transaction, so it was never sent.
	StatusRejected = "REJECTED"
)

var (
	// ErrNotPrepared means the window has no settlement session.
	ErrNotPrepared = errors.New("settlement has not been prepared for this window")
	// ErrInvalid wraps every reason a signed authorization is refused.
	ErrInvalid = errors.New("invalid authorization")
)

// Settler prepares and drives settlement sessions.
type Settler struct {
	RPC        RPC
	Passphrase string        // network passphrase
	Contract   string        // settlement contract, C…
	Operator   *keypair.Full // submits every transaction; the contract's admin, for settle
	Dir        string        // where sessions are kept
	Validity   uint32        // ledgers an authorization stays valid; 0 means DefaultValidity

	mu      sync.Mutex // guards session files
	advance sync.Mutex // one Advance at a time
}

// Session is one window's settlement.
type Session struct {
	Window   uint64   `json:"window"`
	Contract string   `json:"contract"`
	Batches  []*Batch `json:"batches"`
	Settle   *Tx      `json:"settle,omitempty"`
}

// Batch is one submit call.
type Batch struct {
	Obligations []soroban.Obligation `json:"obligations"`
	// Auth is every authorization entry simulation recorded, in order;
	// debtors' address entries are replaced by their signed versions.
	Auth    []string `json:"auth"`
	Entries []*Entry `json:"entries"`
	Tx      *Tx      `json:"tx,omitempty"`
}

// Entry is one debtor authorization to collect.
type Entry struct {
	Slot    int    `json:"slot"`    // index into Batch.Auth
	Address string `json:"address"` // the debtor, G… or C…
	Entry   string `json:"entry"`   // base64 SorobanAuthorizationEntry, unsigned
	// Hash is what an Ed25519 key signs: SHA-256 of the entry's
	// HashIdPreimage on this network, hex.
	Hash             string `json:"hash"`
	ExpirationLedger uint32 `json:"expiration_ledger"`
	Signed           string `json:"signed,omitempty"`
}

// Tx is a transaction's fate.
type Tx struct {
	Hash   string `json:"hash,omitempty"`
	Status string `json:"status"`
	Ledger uint32 `json:"ledger,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Settled reports whether the window is settled on chain.
func (s *Session) Settled() bool { return s.Settle != nil && s.Settle.Status == "SUCCESS" }

// Pending counts the authorizations still to collect.
func (s *Session) Pending() int {
	n := 0
	for _, b := range s.Batches {
		if b.Tx != nil && b.Tx.Status == "SUCCESS" {
			continue
		}
		for _, e := range b.Entries {
			if e.Signed == "" {
				n++
			}
		}
	}
	return n
}

// Signature is a debtor's answer for one entry: either the whole signed
// entry (as Stellar SDKs' authorizeEntry and tessera-coordinator's
// /v1/authorize return it) or, for an account, just the Ed25519 signature of
// the entry's hash.
type Signature struct {
	Batch     int    `json:"batch"`
	Slot      int    `json:"slot"`
	Entry     string `json:"entry,omitempty"`
	Signature string `json:"signature,omitempty"` // hex
}

// Prepare simulates every batch of window w and records the authorizations
// its debtors must sign. Preparing again refreshes the batches not yet
// submitted, with new nonces and expiry, discarding their signatures.
func (s *Settler) Prepare(ctx context.Context, w *clearing.Closed, m soroban.Mapping) (*Session, error) {
	plan, err := soroban.Plan(w, m)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.load(w.Window)
	if err != nil && !errors.Is(err, ErrNotPrepared) {
		return nil, err
	}
	latest, err := s.RPC.LatestLedger(ctx)
	if err != nil {
		return nil, err
	}
	sess := &Session{Window: w.Window, Contract: s.Contract}
	for i, obligations := range plan {
		if old != nil && i < len(old.Batches) && old.Batches[i].Tx != nil && old.Batches[i].Tx.Status == "SUCCESS" {
			sess.Batches = append(sess.Batches, old.Batches[i])
			continue
		}
		b, err := s.prepareBatch(ctx, obligations, latest)
		if err != nil {
			return nil, fmt.Errorf("batch %d: %w", i, err)
		}
		sess.Batches = append(sess.Batches, b)
	}
	return sess, s.save(sess)
}

func (s *Settler) prepareBatch(ctx context.Context, obligations []soroban.Obligation, latest uint32) (*Batch, error) {
	arg, err := obligationsVal(obligations)
	if err != nil {
		return nil, err
	}
	sim, _, err := s.simulate(ctx, "submit", []xdr.ScVal{arg}, nil)
	if err != nil {
		return nil, err
	}
	validity := s.Validity
	if validity == 0 {
		validity = DefaultValidity
	}
	b := &Batch{Obligations: obligations}
	for slot, raw := range sim.Auth {
		var e xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(raw, &e); err != nil {
			return nil, fmt.Errorf("simulation returned an unreadable authorization entry: %w", err)
		}
		if e.Credentials.Type == xdr.SorobanCredentialsTypeSorobanCredentialsAddress {
			e.Credentials.Address.SignatureExpirationLedger = xdr.Uint32(latest + validity)
			address, err := e.Credentials.Address.Address.String()
			if err != nil {
				return nil, err
			}
			hash, err := PreimageHash(s.Passphrase, e)
			if err != nil {
				return nil, err
			}
			if raw, err = xdr.MarshalBase64(e); err != nil {
				return nil, err
			}
			b.Entries = append(b.Entries, &Entry{
				Slot: slot, Address: address, Entry: raw, Hash: hex.EncodeToString(hash[:]), ExpirationLedger: latest + validity,
			})
		}
		b.Auth = append(b.Auth, raw)
	}
	return b, nil
}

// Session returns window n's session.
func (s *Settler) Session(n uint64) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(n)
}

// Accept records signed authorizations from the debtor at address. Either
// all are accepted or none are.
func (s *Settler) Accept(n uint64, address string, sigs []Signature) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(n)
	if err != nil {
		return nil, err
	}
	signed := map[*Entry]string{}
	for i, sig := range sigs {
		e, entry, err := s.check(sess, address, sig)
		if err != nil {
			return nil, fmt.Errorf("%w: authorization %d: %w", ErrInvalid, i, err)
		}
		if signed[e], err = xdr.MarshalBase64(entry); err != nil {
			return nil, err
		}
	}
	for e, v := range signed {
		e.Signed = v
	}
	return sess, s.save(sess)
}

// check validates one signature against the session, returning the slot it
// fills and the signed entry.
func (s *Settler) check(sess *Session, address string, sig Signature) (*Entry, xdr.SorobanAuthorizationEntry, error) {
	var none xdr.SorobanAuthorizationEntry
	if sig.Batch < 0 || sig.Batch >= len(sess.Batches) {
		return nil, none, fmt.Errorf("no batch %d", sig.Batch)
	}
	b := sess.Batches[sig.Batch]
	if b.Tx != nil && b.Tx.Status == "SUCCESS" {
		return nil, none, fmt.Errorf("batch %d is already settled", sig.Batch)
	}
	var want *Entry
	for _, e := range b.Entries {
		if e.Slot == sig.Slot {
			want = e
		}
	}
	switch {
	case want == nil:
		return nil, none, fmt.Errorf("batch %d has no authorization in slot %d", sig.Batch, sig.Slot)
	case want.Address != address:
		return nil, none, fmt.Errorf("batch %d slot %d is not yours to sign", sig.Batch, sig.Slot)
	}
	var unsigned xdr.SorobanAuthorizationEntry
	if err := xdr.SafeUnmarshalBase64(want.Entry, &unsigned); err != nil {
		return nil, none, err
	}
	var entry xdr.SorobanAuthorizationEntry
	switch {
	case sig.Entry != "" && sig.Signature != "":
		return nil, none, errors.New("give the signed entry or the signature, not both")
	case sig.Entry != "":
		if err := xdr.SafeUnmarshalBase64(sig.Entry, &entry); err != nil {
			return nil, none, fmt.Errorf("entry is not a base64 SorobanAuthorizationEntry: %w", err)
		}
		if entry.Credentials.Type != xdr.SorobanCredentialsTypeSorobanCredentialsAddress {
			return nil, none, errors.New("entry has no address credentials")
		}
		// The signed entry must be the unsigned one plus a signature: same
		// address, nonce, expiry and invocation, byte for byte.
		bare := entry
		creds := *entry.Credentials.Address
		creds.Signature = unsigned.Credentials.Address.Signature
		bare.Credentials.Address = &creds
		if !sameXDR(bare, unsigned) {
			return nil, none, errors.New("entry differs from the one issued for this slot")
		}
	case sig.Signature != "":
		if !strkey.IsValidEd25519PublicKey(address) {
			return nil, none, errors.New("a bare signature only works for G… accounts; send the signed entry")
		}
		raw, err := hex.DecodeString(sig.Signature)
		if err != nil || len(raw) != 64 {
			return nil, none, errors.New("signature must be 64 bytes of hex")
		}
		entry = WithSignature(unsigned, address, raw)
	default:
		return nil, none, errors.New("give the signed entry or the signature")
	}
	if strkey.IsValidEd25519PublicKey(address) {
		hash, _ := hex.DecodeString(want.Hash)
		if err := Verify(entry.Credentials.Address.Signature, address, hash); err != nil {
			return nil, none, err
		}
	}
	// Contract accounts check their own signatures; simulation before
	// submission enforces them.
	return want, entry, nil
}

// Advance submits every batch whose authorizations are all signed, then
// settle once every batch has succeeded. It stops at the first failure,
// which is recorded on the session; calling it again retries.
func (s *Settler) Advance(ctx context.Context, n uint64) (*Session, error) {
	s.advance.Lock()
	defer s.advance.Unlock()
	sess, err := s.Session(n)
	if err != nil {
		return nil, err
	}
	for i, b := range sess.Batches {
		if b.Tx != nil && b.Tx.Status == "SUCCESS" {
			continue
		}
		auth := make([]string, len(b.Auth))
		copy(auth, b.Auth)
		ready := true
		for _, e := range b.Entries {
			if e.Signed == "" {
				ready = false
			}
			auth[e.Slot] = e.Signed
		}
		if !ready {
			return sess, nil
		}
		arg, err := obligationsVal(b.Obligations)
		if err != nil {
			return nil, err
		}
		b.Tx = s.send(ctx, "submit", []xdr.ScVal{arg}, auth)
		if err := s.store(sess); err != nil {
			return nil, err
		}
		if b.Tx.Status != "SUCCESS" {
			return sess, fmt.Errorf("batch %d: %s %s", i, b.Tx.Status, b.Tx.Error)
		}
	}
	if !sess.Settled() {
		sess.Settle = s.send(ctx, "settle", nil, nil)
		if err := s.store(sess); err != nil {
			return nil, err
		}
		if !sess.Settled() {
			return sess, fmt.Errorf("settle: %s %s", sess.Settle.Status, sess.Settle.Error)
		}
	}
	return sess, nil
}

// send simulates, signs and submits one contract call. With auth nil, the
// authorizations simulation recorded are used (the operator's own).
func (s *Settler) send(ctx context.Context, fn string, args []xdr.ScVal, auth []string) *Tx {
	entries, err := decodeAuth(auth)
	if err != nil {
		return &Tx{Status: StatusRejected, Error: err.Error()}
	}
	sim, tx, err := s.simulate(ctx, fn, args, entries)
	if err != nil {
		return &Tx{Status: StatusRejected, Error: err.Error()}
	}
	if auth == nil {
		if entries, err = decodeAuth(sim.Auth); err != nil {
			return &Tx{Status: StatusRejected, Error: err.Error()}
		}
	}
	var data xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionData, &data); err != nil {
		return &Tx{Status: StatusRejected, Error: "simulation returned unreadable transaction data"}
	}
	// Leave headroom over the simulated minimum; unused resource fee is refunded.
	data.ResourceFee = xdr.Int64(sim.MinResourceFee + sim.MinResourceFee/5)
	tx, err = s.build(ctx, fn, args, entries, &data, tx)
	if err != nil {
		return &Tx{Status: StatusRejected, Error: err.Error()}
	}
	if tx, err = tx.Sign(s.Passphrase, s.Operator); err != nil {
		return &Tx{Status: StatusRejected, Error: err.Error()}
	}
	hash, _ := tx.HashHex(s.Passphrase)
	env, err := tx.Base64()
	if err != nil {
		return &Tx{Status: StatusRejected, Error: err.Error()}
	}
	out, err := s.RPC.Send(ctx, env, hash)
	if err != nil {
		return &Tx{Hash: hash, Status: StatusRejected, Error: err.Error()}
	}
	return &Tx{Hash: hash, Status: out.Status, Ledger: out.Ledger}
}

// simulate builds the call without resources and simulates it.
func (s *Settler) simulate(ctx context.Context, fn string, args []xdr.ScVal, auth []xdr.SorobanAuthorizationEntry) (*Simulation, *txnbuild.Transaction, error) {
	tx, err := s.build(ctx, fn, args, auth, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	env, err := tx.Base64()
	if err != nil {
		return nil, nil, err
	}
	sim, err := s.RPC.Simulate(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	if sim.Error != "" {
		return nil, nil, fmt.Errorf("simulation failed: %s", sim.Error)
	}
	return sim, tx, nil
}

// build makes the transaction. prev, if given, supplies the sequence number
// so the simulated and the submitted transaction agree.
func (s *Settler) build(ctx context.Context, fn string, args []xdr.ScVal, auth []xdr.SorobanAuthorizationEntry, data *xdr.SorobanTransactionData, prev *txnbuild.Transaction) (*txnbuild.Transaction, error) {
	var seq int64
	if prev != nil {
		seq = prev.SequenceNumber() - 1
	} else {
		var err error
		if seq, err = s.RPC.Sequence(ctx, s.Operator.Address()); err != nil {
			return nil, err
		}
	}
	contract, err := scAddress(s.Contract)
	if err != nil {
		return nil, err
	}
	op := &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type:           xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{ContractAddress: contract, FunctionName: xdr.ScSymbol(fn), Args: args},
		},
		Auth: auth,
	}
	if data != nil {
		op.Ext = xdr.TransactionExt{V: 1, SorobanData: data}
	}
	return txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: s.Operator.Address(), Sequence: seq},
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{op},
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
}

// PreimageHash is what the address in an authorization entry signs on the
// network with passphrase: SHA-256 of its HashIdPreimage.
func PreimageHash(passphrase string, e xdr.SorobanAuthorizationEntry) ([32]byte, error) {
	c := e.Credentials.Address
	if c == nil {
		return [32]byte{}, errors.New("entry has no address credentials")
	}
	pre := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeSorobanAuthorization,
		SorobanAuthorization: &xdr.HashIdPreimageSorobanAuthorization{
			NetworkId:                 sha256.Sum256([]byte(passphrase)),
			Nonce:                     c.Nonce,
			SignatureExpirationLedger: c.SignatureExpirationLedger,
			Invocation:                e.RootInvocation,
		},
	}
	raw, err := pre.MarshalBinary()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// WithSignature returns entry carrying an account's Ed25519 signature in the
// form Stellar accounts check: [{public_key, signature}].
func WithSignature(entry xdr.SorobanAuthorizationEntry, address string, sig []byte) xdr.SorobanAuthorizationEntry {
	pk, _ := strkey.Decode(strkey.VersionByteAccountID, address)
	m := xdr.ScMap{
		{Key: sym("public_key"), Val: bytesVal(pk)},
		{Key: sym("signature"), Val: bytesVal(sig)},
	}
	pm := &m
	vec := xdr.ScVec{{Type: xdr.ScValTypeScvMap, Map: &pm}}
	pv := &vec
	creds := *entry.Credentials.Address
	creds.Signature = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
	entry.Credentials.Address = &creds
	return entry
}

// Verify checks an account's signature ScVal, as WithSignature makes it, against hash.
func Verify(v xdr.ScVal, address string, hash []byte) error {
	bad := errors.New("signature is not [{public_key, signature}] for this account")
	vec, ok := v.GetVec()
	if !ok || vec == nil || len(*vec) != 1 {
		return bad
	}
	m, ok := (*vec)[0].GetMap()
	if !ok || m == nil {
		return bad
	}
	fields := map[string][]byte{}
	for _, e := range *m {
		k, ok1 := e.Key.GetSym()
		b, ok2 := e.Val.GetBytes()
		if ok1 && ok2 {
			fields[string(k)] = b
		}
	}
	pk, _ := strkey.Decode(strkey.VersionByteAccountID, address)
	if !bytes.Equal(fields["public_key"], pk) {
		return errors.New("signature's public key is not the debtor's")
	}
	kp, err := keypair.ParseAddress(address)
	if err != nil {
		return err
	}
	if kp.Verify(hash, fields["signature"]) != nil {
		return errors.New("signature does not verify")
	}
	return nil
}

func decodeAuth(raw []string) ([]xdr.SorobanAuthorizationEntry, error) {
	var out []xdr.SorobanAuthorizationEntry
	for _, r := range raw {
		var e xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(r, &e); err != nil {
			return nil, fmt.Errorf("unreadable authorization entry: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

func sameXDR(a, b xdr.SorobanAuthorizationEntry) bool {
	x, err1 := a.MarshalBinary()
	y, err2 := b.MarshalBinary()
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// obligationsVal encodes a batch as the contract's Vec<Obligation>.
func obligationsVal(obligations []soroban.Obligation) (xdr.ScVal, error) {
	vec := xdr.ScVec{}
	for _, o := range obligations {
		debtor, err := scAddress(o.Debtor)
		if err != nil {
			return xdr.ScVal{}, err
		}
		creditor, err := scAddress(o.Creditor)
		if err != nil {
			return xdr.ScVal{}, err
		}
		token, err := scAddress(o.Token)
		if err != nil {
			return xdr.ScVal{}, err
		}
		amount, ok := new(big.Int).SetString(o.Amount, 10)
		if !ok || amount.Sign() <= 0 || amount.BitLen() > 127 {
			return xdr.ScVal{}, fmt.Errorf("amount %q is not a positive i128", o.Amount)
		}
		ref, err := hex.DecodeString(o.Reference)
		if err != nil || len(ref) != 32 {
			return xdr.ScVal{}, fmt.Errorf("reference %q is not 32 bytes of hex", o.Reference)
		}
		lo := new(big.Int).And(amount, new(big.Int).SetUint64(^uint64(0))).Uint64()
		hi := new(big.Int).Rsh(amount, 64).Int64()
		// A contracttype struct is a map with its field names sorted.
		m := xdr.ScMap{
			{Key: sym("amount"), Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}},
			{Key: sym("creditor"), Val: xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &creditor}},
			{Key: sym("debtor"), Val: xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &debtor}},
			{Key: sym("reference"), Val: bytesVal(ref)},
			{Key: sym("token"), Val: xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &token}},
		}
		pm := &m
		vec = append(vec, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm})
	}
	pv := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}, nil
}

func scAddress(s string) (xdr.ScAddress, error) {
	if strkey.IsValidEd25519PublicKey(s) {
		id, err := xdr.AddressToAccountId(s)
		if err != nil {
			return xdr.ScAddress{}, err
		}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &id}, nil
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, s)
	if err != nil {
		return xdr.ScAddress{}, fmt.Errorf("%q is not a G… or C… address", s)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}, nil
}

func sym(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

func bytesVal(b []byte) xdr.ScVal {
	v := xdr.ScBytes(b)
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &v}
}

func (s *Settler) path(n uint64) string {
	return filepath.Join(s.Dir, "settlement-"+strconv.FormatUint(n, 10)+".json")
}

func (s *Settler) load(n uint64) (*Session, error) {
	raw, err := os.ReadFile(s.path(n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPrepared
	}
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(n), err)
	}
	return &sess, nil
}

// store saves a session Advance has updated, keeping signatures Accept
// recorded meanwhile.
func (s *Settler) store(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, err := s.load(sess.Window); err == nil {
		for i, b := range cur.Batches {
			if i < len(sess.Batches) && len(b.Entries) == len(sess.Batches[i].Entries) {
				for j, e := range b.Entries {
					if sess.Batches[i].Entries[j].Signed == "" {
						sess.Batches[i].Entries[j].Signed = e.Signed
					}
				}
			}
		}
	}
	return s.save(sess)
}

// save writes atomically: a crash leaves the old or the new session, never half.
func (s *Settler) save(sess *Session) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(sess.Window) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(sess.Window))
}
