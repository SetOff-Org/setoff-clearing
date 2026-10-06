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
	Listen         string        `toml:"listen"`
	DataDir        string        `toml:"data_dir"`
	OperatorKeyEnv string        `toml:"operator_key_env"`
	Participants   []Participant `toml:"participant"`
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
func (c *Config) Keys() (map[string]string, []string, string, error) {
	keys := map[string]string{}
	var ids []string
	for _, p := range c.Participants {
		k := os.Getenv(p.KeyEnv)
		if k == "" {
			return nil, nil, "", fmt.Errorf("participant %s: $%s is empty", p.ID, p.KeyEnv)
		}
		keys[k] = p.ID
		ids = append(ids, p.ID)
	}
	op := os.Getenv(c.OperatorKeyEnv)
	if op == "" {
		return nil, nil, "", fmt.Errorf("$%s is empty", c.OperatorKeyEnv)
	}
	return keys, ids, op, nil
}
