package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

const (
	contract = "CCV7S3FEDA5TXR6IKS3R2PE3UZVET2U76SCTGBI3WDE6EKINKDV4WNGU"
	usdc     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

func addr(i int) string { return fmt.Sprintf("G%055d", i) }

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`data_dir = "data"
operator_key_env = "OP"
contract = %q

[tokens]
USDC = %q

[[participant]]
id = "a"
key_env = "KA"
address = %q

[[participant]]
id = "b"
key_env = "KB"
address = %q
`, contract, usdc, addr(1), addr(2))
	path := filepath.Join(dir, "setoff.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := clearing.Open(filepath.Join(dir, "data"), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []clearing.Submission{
		{Reference: "1", Creditor: "b", Asset: "USDC", Amount: netting.NewAmount(100)},
	} {
		if _, _, err := c.Submit("a", s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Close(time.Now()); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSorobanPrintsTheCalls(t *testing.T) {
	path := setup(t)
	var out bytes.Buffer
	if err := runSoroban([]string{"--config", path, "--window", "1", "--network", "testnet"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"stellar contract invoke --id " + contract + " --source operator --network testnet -- submit --obligations '[{",
		`"debtor":"` + addr(1) + `"`,
		`"amount":"100"`,
		"-- settle\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestSorobanRefusesBadInput(t *testing.T) {
	path := setup(t)
	for name, args := range map[string][]string{
		"no window":      {"--config", path},
		"unknown window": {"--config", path, "--window", "9"},
	} {
		if err := runSoroban(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestReportWritesCamt053(t *testing.T) {
	path := setup(t)
	var out bytes.Buffer
	if err := runReport([]string{"--config", path, "--window", "1", "--participant", "b"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "camt.053.001.08") || strings.Count(s, "<Stmt>") != 1 || !strings.Contains(s, "<CdtDbtInd>CRDT</CdtDbtInd>") {
		t.Fatalf("%s", s)
	}
	if err := runReport([]string{"--config", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("--window is required")
	}
}
