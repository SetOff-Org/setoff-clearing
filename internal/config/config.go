// Package config loads setoff.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Participant is one [[participant]] entry.
type Participant struct {
	ID     string `toml:"id"`
	KeyEnv string `toml:"key_env"`
	// Address is the participant's Stellar account or contract, for on-chain settlement.
	Address string `toml:"address"`
}

// Config is setoff.toml.
type Config struct {
	Listen         string `toml:"listen"`
	DataDir        string `toml:"data_dir"`
	OperatorKeyEnv string `toml:"operator_key_env"`
	// Assets lists the asset codes obligations may use. Empty allows any.
	Assets []string `toml:"assets"`
	// Decimals per asset, used to express amounts in reports.
	Decimals map[string]int `toml:"decimals"`
	// Contract is the SetOff settlement contract windows settle through.
	Contract string `toml:"contract"`
	// CloseEvery closes the open window on a schedule, e.g. "1h". Empty
	// windows are skipped. Unset: only the operator closes windows.
	CloseEvery Duration `toml:"close_every"`
	// SEP10, if set, lets participants sign in with their Stellar accounts
	// (the address on each [[participant]]) instead of API keys.
	SEP10 struct {
		HomeDomain     string `toml:"home_domain"`
		Network        string `toml:"network"`
		SigningKeyEnv  string `toml:"signing_key_env"`
		TokenSecretEnv string `toml:"token_secret_env"`
	} `toml:"sep10"`
	// Webhook, if set, is told about every closed window.
	Webhook struct {
		URL       string `toml:"url"`
		SecretEnv string `toml:"secret_env"`
	} `toml:"webhook"`
	// Settlement, if set, collects debtors' authorizations over the API and
	// submits each closed window to the contract.
	Settlement Settlement `toml:"settlement"`
	// Tokens maps each asset to its SEP-41 token contract on chain.
	Tokens       map[string]string `toml:"tokens"`
	Participants []Participant     `toml:"participant"`
}

// Settlement is the [settlement] table.
type Settlement struct {
	RPC               string `toml:"rpc"`
	Network           string `toml:"network"`             // testnet, public, or a passphrase
	OperatorSecretEnv string `toml:"operator_secret_env"` // S… key of the account that submits
	// ValidityLedgers is how long debtors have to sign, in ledgers (about 5 s).
	ValidityLedgers uint32 `toml:"validity_ledgers"`
	// PrepareOnClose prepares settlement as soon as a window closes.
	PrepareOnClose bool `toml:"prepare_on_close"`
	// PositionQuota is the contract's position_quota; 0 means its default.
	PositionQuota int `toml:"position_quota"`
}

// Addresses maps participant ids to their Stellar addresses, where set.
func (c *Config) Addresses() map[string]string {
	out := map[string]string{}
	for _, p := range c.Participants {
		if p.Address != "" {
			out[p.ID] = p.Address
		}
	}
	return out
}

// Duration is a TOML string such as "15m".
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Load reads a config file. data_dir is resolved relative to it.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, und)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7500"
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("%s: data_dir is required", path)
	}
	if d := c.CloseEvery.Duration; d != 0 && d < time.Minute {
		return nil, fmt.Errorf("%s: close_every must be at least 1m", path)
	}
	if st := c.Settlement; st.RPC != "" {
		switch {
		case len(c.Contract) != 56 || c.Contract[0] != 'C':
			return nil, fmt.Errorf("%s: [settlement] needs contract, the settlement contract's C… address", path)
		case st.Network == "":
			return nil, fmt.Errorf("%s: [settlement] needs network", path)
		case st.OperatorSecretEnv == "":
			return nil, fmt.Errorf("%s: [settlement] needs operator_secret_env", path)
		case len(c.Tokens) == 0:
			return nil, fmt.Errorf("%s: [settlement] needs [tokens]", path)
		}
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
	}
	return &c, nil
}

// Keys reads every key from the environment. It returns the participant
// keys (key -> participant id), the participant ids, and the operator key.
//
// Keys are how callers are identified, so each must belong to exactly one
// party: a shared key would let one participant act as another, or as the
// operator.
func (c *Config) Keys() (map[string]string, []string, string, error) {
	op := os.Getenv(c.OperatorKeyEnv)
	if op == "" {
		return nil, nil, "", fmt.Errorf("$%s is empty", c.OperatorKeyEnv)
	}
	keys := map[string]string{}
	seen := map[string]bool{}
	var ids []string
	for _, p := range c.Participants {
		if p.ID == "" {
			return nil, nil, "", fmt.Errorf("a participant has no id")
		}
		if seen[p.ID] {
			return nil, nil, "", fmt.Errorf("participant %s is listed twice", p.ID)
		}
		seen[p.ID] = true
		k := os.Getenv(p.KeyEnv)
		switch {
		case k == "":
			return nil, nil, "", fmt.Errorf("participant %s: $%s is empty", p.ID, p.KeyEnv)
		case k == op:
			return nil, nil, "", fmt.Errorf("participant %s holds the operator key", p.ID)
		case keys[k] != "":
			return nil, nil, "", fmt.Errorf("participants %s and %s share a key", keys[k], p.ID)
		}
		keys[k] = p.ID
		ids = append(ids, p.ID)
	}
	return keys, ids, op, nil
}
