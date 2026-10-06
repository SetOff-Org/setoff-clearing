package soroban

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

func addr(i int) string { return fmt.Sprintf("G%055d", i) }

func closed(t *testing.T, obs []netting.Obligation) *clearing.Closed {
	t.Helper()
	res, err := netting.Net(obs)
	if err != nil {
		t.Fatal(err)
	}
	return &clearing.Closed{Window: 3, ClosedAt: time.Now(), Obligations: obs, Netting: res}
}

func TestPlanSubmitsTheNettedLegs(t *testing.T) {
	w := closed(t, []netting.Obligation{
		{ID: "1", Debtor: "a", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(100)},
		{ID: "2", Debtor: "b", Creditor: "c", Asset: "USDC", Amount: netting.NewAmount(100)},
		{ID: "3", Debtor: "c", Creditor: "a", Asset: "USDC", Amount: netting.NewAmount(30)},
	})
	m := Mapping{Addresses: map[string]string{"a": addr(1), "b": addr(2), "c": addr(3)}, Tokens: map[string]string{"USDC": usdc}}
	batches, err := Plan(w, m)
	if err != nil {
		t.Fatal(err)
	}
	// Three gross obligations, one leg: a pays c 70.
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("%+v", batches)
	}
	leg := batches[0][0]
	if leg.Debtor != addr(1) || leg.Creditor != addr(3) || leg.Amount != "70" || leg.Token != usdc || leg.Reference != Reference(3, 0) {
		t.Fatalf("%+v", leg)
	}
	if Reference(3, 0) == Reference(4, 0) || Reference(3, 0) == Reference(3, 1) || len(Reference(3, 0)) != 64 {
		t.Fatal("references must be distinct 32-byte hex per window and leg")
	}
}

func TestPlanNeedsEveryAddressAndToken(t *testing.T) {
	w := closed(t, []netting.Obligation{{ID: "1", Debtor: "a", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(5)}})
	for name, m := range map[string]Mapping{
		"debtor":   {Addresses: map[string]string{"b": addr(2)}, Tokens: map[string]string{"USDC": usdc}},
		"creditor": {Addresses: map[string]string{"a": addr(1), "b": "nope"}, Tokens: map[string]string{"USDC": usdc}},
		"token":    {Addresses: map[string]string{"a": addr(1), "b": addr(2)}, Tokens: map[string]string{"USDC": addr(9)}},
	} {
		if _, err := Plan(w, m); err == nil {
			t.Errorf("%s: planned without a valid mapping", name)
		}
	}
}

func TestPlanRespectsContractLimits(t *testing.T) {
	// 40 debtors each paying one creditor: 40 legs, 41 positions.
	var obs []netting.Obligation
	addresses := map[string]string{"sink": addr(0)}
	for i := 1; i <= 40; i++ {
		id := fmt.Sprintf("p%d", i)
		addresses[id] = addr(i)
		obs = append(obs, netting.Obligation{ID: id, Debtor: id, Creditor: "sink", Asset: "USDC", Amount: netting.NewAmount(int64(i))})
	}
	m := Mapping{Addresses: addresses, Tokens: map[string]string{"USDC": usdc}}
	batches, err := Plan(closed(t, obs), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != MaxBatch || len(batches[1]) != 8 {
		t.Fatalf("batches of %d and %d", len(batches[0]), len(batches[1]))
	}

	// 70 debtors: more positions than one contract window holds.
	for i := 41; i <= 70; i++ {
		id := fmt.Sprintf("p%d", i)
		addresses[id] = addr(i)
		obs = append(obs, netting.Obligation{ID: id, Debtor: id, Creditor: "sink", Asset: "USDC", Amount: netting.NewAmount(1)})
	}
	if _, err := Plan(closed(t, obs), m); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Fatalf("want a positions error, got %v", err)
	}
}

func TestAWindowThatNetsToZeroNeedsNothingOnChain(t *testing.T) {
	w := closed(t, []netting.Obligation{
		{ID: "1", Debtor: "a", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(10)},
		{ID: "2", Debtor: "b", Creditor: "a", Asset: "USDC", Amount: netting.NewAmount(10)},
	})
	if _, err := Plan(w, Mapping{}); !errors.Is(err, ErrNothingToSettle) {
		t.Fatalf("want ErrNothingToSettle, got %v", err)
	}
}

func TestPlanRespectsThePositionQuota(t *testing.T) {
	// One hub pays 20 members: 21 new positions on the hub's account.
	var obs []netting.Obligation
	addresses := map[string]string{"hub": addr(0)}
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("m%d", i)
		addresses[id] = addr(i)
		obs = append(obs, netting.Obligation{ID: id, Debtor: "hub", Creditor: id, Asset: "USDC", Amount: netting.NewAmount(1)})
	}
	m := Mapping{Addresses: addresses, Tokens: map[string]string{"USDC": usdc}}
	if _, err := Plan(closed(t, obs), m); err == nil || !strings.Contains(err.Error(), "position_quota of 16") {
		t.Fatalf("want a quota error, got %v", err)
	}
	m.PositionQuota = 32
	if _, err := Plan(closed(t, obs), m); err != nil {
		t.Fatal(err)
	}
}
