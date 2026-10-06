package clearing

import (
	"errors"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

var members = []string{"a", "b", "c"}

func open(t *testing.T, dir string) *Clearing {
	t.Helper()
	c, err := Open(dir, members)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func owe(t *testing.T, c *Clearing, debtor, creditor, ref string, amount int64) {
	t.Helper()
	if _, _, err := c.Submit(debtor, Submission{Reference: ref, Creditor: creditor, Asset: "USDC", Amount: netting.NewAmount(amount)}); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, c *Clearing) (uint64, int) {
	t.Helper()
	w, n, _, err := c.Preview()
	if err != nil {
		t.Fatal(err)
	}
	return w, n
}

func TestObligationsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	owe(t, c, "a", "b", "inv-1", 100)
	owe(t, c, "b", "c", "inv-1", 40) // references are per debtor

	c = open(t, dir)
	if w, n := count(t, c); w != 1 || n != 2 {
		t.Fatalf("window %d with %d obligations after restart", w, n)
	}
	if _, _, err := c.Submit("a", Submission{Reference: "inv-1", Creditor: "c", Asset: "USDC", Amount: netting.NewAmount(1)}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a reference must stay used across restarts, got %v", err)
	}
}

func TestCloseArchivesAndStartsAFreshWindow(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	owe(t, c, "a", "b", "1", 100)
	owe(t, c, "b", "a", "1", 60)
	closed, err := c.Close(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Window != 1 || len(closed.Obligations) != 2 || len(closed.Netting.Transfers) != 1 {
		t.Fatalf("%+v", closed)
	}
	if w, n := count(t, c); w != 2 || n != 0 {
		t.Fatalf("window %d holds %d obligations after close", w, n)
	}
	// The archive is durable and the numbering survives a restart.
	c = open(t, dir)
	if w, _ := count(t, c); w != 2 {
		t.Fatalf("restart reopened window %d", w)
	}
	got, err := c.Window(1)
	if err != nil || got.Netting.Transfers[0].Amount.String() != "40" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := c.Window(2); !errors.Is(err, ErrUnknownWindow) {
		t.Fatalf("the open window is not archived yet: %v", err)
	}
	// A reference may be reused in a later window.
	owe(t, c, "a", "b", "1", 5)
}

func TestSubmissionsAreValidated(t *testing.T) {
	c := open(t, t.TempDir())
	for name, s := range map[string]Submission{
		"no reference":   {Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(1)},
		"long reference": {Reference: string(make([]byte, 65)), Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(1)},
		"stranger":       {Reference: "r", Creditor: "z", Asset: "USDC", Amount: netting.NewAmount(1)},
		"zero":           {Reference: "r", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(0)},
		"self":           {Reference: "r", Creditor: "a", Asset: "USDC", Amount: netting.NewAmount(1)},
	} {
		if _, _, err := c.Submit("a", s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, n := count(t, c); n != 0 {
		t.Fatalf("%d invalid obligations recorded", n)
	}
}
