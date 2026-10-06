package clearing

import (
	"fmt"
	"testing"

	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

// BenchmarkSubmitIntoLargeWindow measures one submission into a window that
// already holds 5,000 obligations (journal writes included).
func BenchmarkSubmitIntoLargeWindow(b *testing.B) {
	c, err := Open(b.TempDir(), []string{"a", "b", "c", "d"})
	if err != nil {
		b.Fatal(err)
	}
	for i := range 5_000 {
		o := netting.Obligation{ID: fmt.Sprintf("a:%d", i), Debtor: "a", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(int64(i + 1))}
		c.admit(o)
	}
	b.ResetTimer()
	for i := range b.N {
		if _, _, err := c.Submit("c", Submission{Reference: fmt.Sprint(i), Creditor: "d", Asset: "USDC", Amount: netting.NewAmount(1)}); err != nil {
			b.Fatal(err)
		}
	}
}
