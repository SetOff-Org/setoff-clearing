// Package soroban turns a closed window into calls on the SetOff settlement
// contract (github.com/SetOff-Org/setoff-contracts).
//
// The contract nets on chain, but there is no need to send it every gross
// obligation: the window's settlement plan has the same net positions with at
// most one leg fewer than participants per asset. Submitting the plan instead
// keeps the on-chain footprint, and fees, small.
package soroban

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
)

// Limits of the settlement contract.
const (
	MaxBatch     = 32 // obligations per submit call
	MaxPositions = 64 // (member, token) positions per contract window
	// DefaultPositionQuota is the contract's default for how many new
	// positions one debtor's obligations may open per window.
	DefaultPositionQuota = 16
)

// Obligation is the contract's Obligation struct, in the JSON form the
// Stellar CLI accepts for it (i128 as a string, BytesN<32> as hex).
type Obligation struct {
	Debtor    string `json:"debtor"`
	Creditor  string `json:"creditor"`
	Token     string `json:"token"`
	Amount    string `json:"amount"`
	Reference string `json:"reference"`
}

// Mapping names participants and assets on chain.
type Mapping struct {
	Addresses map[string]string // participant id -> G… or C… address
	Tokens    map[string]string // asset -> SEP-41 token contract C…
	// PositionQuota is the contract's position_quota; 0 means the default.
	PositionQuota int
}

// Plan returns the submit batches that settle window w on chain. Each leg's
// reference is derived from the window and leg number, so resubmitting a
// batch after a timeout is rejected by the contract instead of booked twice.
func Plan(w *clearing.Closed, m Mapping) ([][]Obligation, error) {
	if len(w.Netting.Transfers) == 0 {
		return nil, ErrNothingToSettle
	}
	var legs []Obligation
	positions := map[[2]string]bool{}
	opened := map[string]int{} // new positions per debtor, as the contract counts them
	for i, t := range w.Netting.Transfers {
		debtor, creditor, token := m.Addresses[t.From], m.Addresses[t.To], m.Tokens[t.Asset]
		switch {
		case !address(debtor, "G", "C"):
			return nil, fmt.Errorf("participant %s has no Stellar address", t.From)
		case !address(creditor, "G", "C"):
			return nil, fmt.Errorf("participant %s has no Stellar address", t.To)
		case !address(token, "C"):
			return nil, fmt.Errorf("asset %s has no token contract", t.Asset)
		}
		for _, p := range [][2]string{{debtor, token}, {creditor, token}} {
			if !positions[p] {
				positions[p] = true
				opened[debtor]++
			}
		}
		legs = append(legs, Obligation{
			Debtor: debtor, Creditor: creditor, Token: token, Amount: t.Amount.String(),
			Reference: Reference(w.Window, i),
		})
	}
	if len(positions) > MaxPositions {
		return nil, fmt.Errorf("window %d touches %d positions; the contract settles at most %d per window", w.Window, len(positions), MaxPositions)
	}
	quota := m.PositionQuota
	if quota == 0 {
		quota = DefaultPositionQuota
	}
	for debtor, n := range opened {
		if n > quota {
			return nil, fmt.Errorf("window %d: %s would open %d positions, over the contract's position_quota of %d; raise it with set_position_quota", w.Window, debtor, n, quota)
		}
	}
	var batches [][]Obligation
	for len(legs) > 0 {
		n := min(len(legs), MaxBatch)
		batches = append(batches, legs[:n])
		legs = legs[n:]
	}
	return batches, nil
}

// Reference is the on-chain idempotency key of leg i of a window.
func Reference(window uint64, leg int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "setoff/window/%d/leg/%d", window, leg))
	return hex.EncodeToString(sum[:])
}

// address loosely checks a strkey: 56 characters with one of the prefixes.
func address(s string, prefixes ...string) bool {
	if len(s) != 56 {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ErrNothingToSettle means the window nets to zero: nothing to submit.
var ErrNothingToSettle = errors.New("the window nets to zero")
