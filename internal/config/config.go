// Package config loads setoff.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Participant is one [[participant]] entry.
type Participant struct {
	ID     string `toml:"id"`
	KeyEnv string `toml:"key_env"`
}

// Config is setoff.toml.
type Config struct {
	Listen         string `toml:"listen"`
	DataDir        string `toml:"data_dir"`
	OperatorKeyEnv string `toml:"operator_key_env"`
	// Assets lists the asset codes obligations may use. Empty allows any.
	Assets       []string      `toml:"assets"`
	Participants []Participant `toml:"participant"`
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
