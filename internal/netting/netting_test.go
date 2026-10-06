package netting

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type vector struct {
	Name        string          `json:"name"`
	Obligations []Obligation    `json:"obligations"`
	Netting     json.RawMessage `json:"netting"`
}

// Every reference vector produced by the Rust engine must come out identical here.
func TestMatchesRustReferenceVectors(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/vectors/*.json")
	if len(files) == 0 {
		t.Fatal("no vectors found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var v vector
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			got, err := Net(v.Obligations)
			if err != nil {
				t.Fatal(err)
			}
			var want Result
			if err := json.Unmarshal(v.Netting, &want); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("Go and Rust disagree on %s", v.Name)
			}
		})
	}
}

func TestOrderNeverMatters(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/vectors/generated-200.json")
	var v vector
	_ = json.Unmarshal(b, &v)
	want, _ := Net(v.Obligations)
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		r.Shuffle(len(v.Obligations), func(i, j int) { v.Obligations[i], v.Obligations[j] = v.Obligations[j], v.Obligations[i] })
		got, err := Net(v.Obligations)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("shuffled input changed the result")
		}
	}
}

func TestInvalidWindows(t *testing.T) {
	ob := func(id, d, c string, amt int64) Obligation {
		return Obligation{ID: id, Debtor: d, Creditor: c, Asset: "X", Amount: NewAmount(amt)}
	}
	max, _ := ParseAmount("170141183460469231731687303715884105727")
	cases := map[string][]Obligation{
		"zero":      {ob("1", "A", "B", 0)},
		"self":      {ob("1", "A", "A", 5)},
		"duplicate": {ob("1", "A", "B", 5), ob("1", "B", "C", 5)},
		"empty":     {ob("1", "", "B", 5)},
		"overflow":  {{ID: "1", Debtor: "A", Creditor: "B", Asset: "X", Amount: max}, ob("2", "C", "B", 1)},
	}
	for name, obs := range cases {
		if _, err := Net(obs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAmountsAreCanonical(t *testing.T) {
	for _, bad := range []string{"", "+5", "05", "-0", "1.5", "1e3", "170141183460469231731687303715884105728"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if a, err := ParseAmount("-42"); err != nil || a.Int64() != -42 {
		t.Fatal("-42 rejected")
	}
}

func TestSavings(t *testing.T) {
	s := AssetSummary{Gross: NewAmount(160), Settled: NewAmount(40)}
	if s.SavingBPS() != 7_500 {
		t.Fatalf("got %d bps", s.SavingBPS())
	}
}
