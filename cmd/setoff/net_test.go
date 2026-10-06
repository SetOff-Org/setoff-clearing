package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

func file(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadObligationsAcceptsCSVAndJSON(t *testing.T) {
	csv := file(t, "w.csv", "id,debtor,creditor,asset,amount\n1,a,b,USDC,100\n2,b,a,USDC,40\n")
	json := file(t, "w.json", `[{"id":"1","debtor":"a","creditor":"b","asset":"USDC","amount":"100"},
		{"id":"2","debtor":"b","creditor":"a","asset":"USDC","amount":"40"}]`)
	for _, p := range []string{csv, json} {
		obs, err := readObligations(p)
		if err != nil || len(obs) != 2 || obs[1].Amount.String() != "40" {
			t.Fatalf("%s: %+v %v", p, obs, err)
		}
	}
}

func TestReadObligationsRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"wrong header":  "debtor,creditor,amount\na,b,1\n",
		"bad amount":    "id,debtor,creditor,asset,amount\n1,a,b,USDC,1.5\n",
		"ragged row":    "id,debtor,creditor,asset,amount\n1,a,b\n",
		"broken JSON":   `[{"id":"1",`,
		"empty":         "",
		"negative JSON": `[{"id":"1","debtor":"a","creditor":"b","asset":"USDC","amount":"-5"}]`,
	} {
		obs, err := readObligations(file(t, "in", body))
		if err == nil {
			// Amount parsing accepts signs; netting is what rejects them.
			if _, nerr := runNetOn(obs); nerr == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	}
}

func TestNetReportsSavings(t *testing.T) {
	p := file(t, "w.csv", "id,debtor,creditor,asset,amount\n1,a,b,USDC,100\n2,b,c,USDC,100\n3,c,a,USDC,100\n")
	out, err := capture(func() error { return runNet([]string{p}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "USDC   3            300    0        100.00%  0") {
		t.Fatalf("a perfect cycle saves everything:\n%s", out)
	}
}

// runNetOn nets already-read obligations, as runNet does after reading.
func runNetOn(obs []netting.Obligation) (*netting.Result, error) { return netting.Net(obs) }

// capture runs f with stdout redirected and returns what it printed.
func capture(f func() error) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	old := os.Stdout
	os.Stdout = w
	ferr := f()
	_ = w.Close()
	os.Stdout = old
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String(), ferr
}
