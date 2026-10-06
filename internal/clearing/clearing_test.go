package clearing

import (
	"errors"
	"os"
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

// A crash after Close archived a window but before it removed the journal
// must not carry the closed window's obligations into the next one, where
// they would settle a second time.
func TestACrashDuringCloseNeverSettlesTwice(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	owe(t, c, "a", "b", "1", 100)
	journal, err := os.ReadFile(c.journal())
	if err != nil {
		t.Fatal(err)
	}
	stale := c.journal()
	if _, err := c.Close(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Put the journal back as if the process died before removing it.
	if err := os.WriteFile(stale, journal, 0o600); err != nil {
		t.Fatal(err)
	}

	c = open(t, dir)
	if w, n := count(t, c); w != 2 || n != 0 {
		t.Fatalf("window %d reopened with %d obligations from the closed window", w, n)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale journal should be removed, got %v", err)
	}
}

// A crash mid-append leaves a partial last line. That obligation was never
// acknowledged (acknowledgement follows fsync of a whole line), so the store
// drops it and starts, rather than refusing to start at all.
func TestATornJournalLineIsDropped(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	owe(t, c, "a", "b", "1", 100)
	f, err := os.OpenFile(c.journal(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"a:2","debtor":"a","cred`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	c = open(t, dir)
	if _, n := count(t, c); n != 1 {
		t.Fatalf("%d obligations after a torn write, want 1", n)
	}
	// The journal is usable again: the next append lands on its own line.
	owe(t, c, "a", "b", "2", 5)
	if _, n := count(t, open(t, dir)); n != 2 {
		t.Fatalf("%d obligations, want 2", n)
	}
}

func TestCorruptionInsideTheJournalIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	owe(t, c, "a", "b", "1", 100)
	path := c.journal()
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append([]byte("{not json}\n"), good...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, members); err == nil {
		t.Fatal("a complete but unreadable line is corruption, not a torn write")
	}
}

func TestAWindowCannotOverflowAtClose(t *testing.T) {
	c := open(t, t.TempDir())
	big, err := netting.ParseAmount("100000000000000000000000000000000000000") // 1e38; i128 max is about 1.7e38
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Submit("a", Submission{Reference: "1", Creditor: "b", Asset: "USDC", Amount: big}); err != nil {
		t.Fatal(err)
	}
	// Different parties, same asset: the gross total is what overflows.
	if _, _, err := c.Submit("c", Submission{Reference: "2", Creditor: "a", Asset: "USDC", Amount: big}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for an overflowing window, got %v", err)
	}
	// Another asset has its own total.
	if _, _, err := c.Submit("c", Submission{Reference: "3", Creditor: "a", Asset: "EURC", Amount: big}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Close(time.Now()); err != nil {
		t.Fatalf("the window must still net: %v", err)
	}
}

func TestAssetsCanBeRestricted(t *testing.T) {
	c := open(t, t.TempDir())
	owe(t, c, "a", "b", "before", 1) // any asset until restricted
	c.RestrictAssets([]string{"USDC", "EURC"})
	owe(t, c, "a", "b", "usdc", 1)
	if _, _, err := c.Submit("a", Submission{Reference: "typo", Creditor: "b", Asset: "USCD", Amount: netting.NewAmount(1)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a mistyped asset must be refused, got %v", err)
	}
}

func TestMineShowsOnlyTheParticipantsSide(t *testing.T) {
	c := open(t, t.TempDir())
	owe(t, c, "a", "b", "1", 100)
	owe(t, c, "b", "c", "1", 70)
	owe(t, c, "c", "a", "1", 20)
	v, err := c.Mine("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Obligations) != 2 || len(v.Positions) != 1 || v.Positions[0].Net.String() != "-80" {
		t.Fatalf("%+v", v)
	}
	for _, tr := range v.Transfers {
		if tr.From != "a" && tr.To != "a" {
			t.Fatalf("leg %+v does not involve a", tr)
		}
	}
	if v, _ := c.Mine("nobody"); len(v.Obligations)+len(v.Positions)+len(v.Transfers) != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestWindowsListsArchivesNewestFirst(t *testing.T) {
	c := open(t, t.TempDir())
	for i := range 3 {
		owe(t, c, "a", "b", "x", int64(i+1))
		if _, err := c.Close(time.Date(2026, 10, 6, i, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := c.Windows(0, 10)
	if err != nil || len(all) != 3 || all[0].Window != 3 || all[2].Window != 1 || all[0].Obligations != 1 {
		t.Fatalf("%+v %v", all, err)
	}
	page, _ := c.Windows(3, 1)
	if len(page) != 1 || page[0].Window != 2 {
		t.Fatalf("page after 3: %+v", page)
	}
}
