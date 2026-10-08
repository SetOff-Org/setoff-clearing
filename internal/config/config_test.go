package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "setoff.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `
data_dir = "data"
operator_key_env = "OP_KEY"

[[participant]]
id = "anchor-ng"
key_env = "KEY_NG"

[[participant]]
id = "anchor-us"
key_env = "KEY_US"
`

func TestLoad(t *testing.T) {
	p := write(t, valid)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:7500" || c.DataDir != filepath.Join(filepath.Dir(p), "data") || len(c.Participants) != 2 {
		t.Fatalf("%+v", c)
	}
	for name, body := range map[string]string{
		"unknown key": valid + "\nport = 1\n",
		"no data_dir": strings.Replace(valid, `data_dir = "data"`, "", 1),
		"bad toml":    "data_dir = ",
		"bad close":   "close_every = \"soon\"\n" + valid,
		"close < 1m":  "close_every = \"5s\"\n" + valid,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestKeys(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_NG", "k-ng")
	t.Setenv("KEY_US", "k-us")
	t.Setenv("OP_KEY", "k-op")
	keys, ids, op, err := c.Keys()
	if err != nil || keys["k-ng"] != "anchor-ng" || len(ids) != 2 || op != "k-op" {
		t.Fatal(keys, ids, op, err)
	}
	t.Setenv("KEY_US", "")
	if _, _, _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "KEY_US") {
		t.Fatalf("an empty key must be named: %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := Load("../../examples/setoff.toml"); err != nil {
		t.Fatal(err)
	}
}

// Keys identify callers, so they must be unambiguous: a shared key would let
// one participant act as another, or as the operator.
func TestKeysMustBeUnambiguous(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OP_KEY", "k-op")
	t.Setenv("KEY_NG", "same")
	t.Setenv("KEY_US", "same")
	if _, _, _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "share a key") {
		t.Fatalf("two participants with one key: %v", err)
	}
	t.Setenv("KEY_US", "k-op")
	if _, _, _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("a participant holding the operator key: %v", err)
	}

	dup, err := Load(write(t, valid+"\n[[participant]]\nid = \"anchor-ng\"\nkey_env = \"KEY_X\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_US", "k-us")
	t.Setenv("KEY_X", "k-x")
	if _, _, _, err := dup.Keys(); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("a participant listed twice: %v", err)
	}
}

func TestSettlement(t *testing.T) {
	const contract = "contract = \"CCW6QCOSJTTJHDXJOVQ36NVIBAHBUVPMSZNOIJ3A444O6YMVHR4ZEYQV\"\n"
	const tokens = "[tokens]\nXLM = \"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC\"\n"
	settlement := func(lines string) string {
		return "[settlement]\nrpc = \"https://rpc.example.org\"\n" + lines
	}
	full := settlement("network = \"testnet\"\noperator_secret_env = \"OP_SECRET\"\nvalidity_ledgers = 120\nprepare_on_close = true\n")
	c, err := Load(write(t, contract+valid+tokens+full))
	if err != nil {
		t.Fatal(err)
	}
	if st := c.Settlement; st.ValidityLedgers != 120 || !st.PrepareOnClose || st.OperatorSecretEnv != "OP_SECRET" {
		t.Fatalf("%+v", st)
	}
	for name, body := range map[string]string{
		"no contract": valid + tokens + full,
		"no tokens":   contract + valid + full,
		"no network":  contract + valid + tokens + settlement("operator_secret_env = \"OP_SECRET\"\n"),
		"no secret":   contract + valid + tokens + settlement("network = \"testnet\"\n"),
	} {
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), "settlement") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
