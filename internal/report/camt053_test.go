package report

import (
	"bytes"
	"encoding/xml"
	"flag"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

var update = flag.Bool("update", false, "rewrite golden files")

func window(t *testing.T) *clearing.Closed {
	t.Helper()
	obs := []netting.Obligation{
		{ID: "anchor-ng:inv-1", Debtor: "anchor-ng", Creditor: "anchor-us", Asset: "USDC", Amount: netting.NewAmount(1_500_000_000)},
		{ID: "anchor-us:inv-9", Debtor: "anchor-us", Creditor: "anchor-ng", Asset: "USDC", Amount: netting.NewAmount(500_000_000)},
		{ID: "anchor-ng:fx-2", Debtor: "anchor-ng", Creditor: "anchor-us", Asset: "EUR", Amount: netting.NewAmount(250)},
	}
	res, err := netting.Net(obs)
	if err != nil {
		t.Fatal(err)
	}
	return &clearing.Closed{Window: 7, ClosedAt: time.Date(2026, 10, 6, 17, 0, 0, 0, time.UTC), Obligations: obs, Netting: res}
}

func TestCamt053Golden(t *testing.T) {
	out, err := Camt053(window(t), Options{
		Decimals: map[string]int{"USDC": 7, "EUR": 2},
		Now:      time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/window-7.camt053.xml"
	if *update {
		if err := os.WriteFile(golden, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("camt.053 output changed; rerun with -update if intended:\n%s", out)
	}
	// It is well-formed XML in the camt.053.001.08 namespace.
	var doc struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(out, &doc); err != nil || doc.XMLName.Space != Namespace {
		t.Fatalf("%v %v", doc.XMLName, err)
	}
}

func TestParticipantsSeeOnlyTheirStatements(t *testing.T) {
	out, err := Camt053(window(t), Options{Participant: "anchor-us", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, "<Stmt>") != 2 || strings.Contains(s, "<Id>anchor-ng</Id>") {
		t.Fatalf("expected anchor-us's two statements only:\n%s", s)
	}
}

func TestDecimal(t *testing.T) {
	for _, c := range []struct {
		v        int64
		decimals int
		want     string
	}{
		{1_500_000_000, 7, "150"},
		{1_234_567, 7, "0.1234567"},
		{5, 2, "0.05"},
		{120, 2, "1.2"},
		{0, 7, "0"},
		{42, 0, "42"},
		{-250, 2, "2.5"}, // the sign goes in CdtDbtInd
	} {
		if got := decimal(big.NewInt(c.v), c.decimals); got != c.want {
			t.Errorf("decimal(%d, %d) = %s, want %s", c.v, c.decimals, got, c.want)
		}
	}
}
