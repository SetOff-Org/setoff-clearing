package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/config"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

func runSoroban(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("soroban", flag.ContinueOnError)
	path := fs.String("config", "setoff.toml", "config file")
	window := fs.Uint64("window", 0, "closed window to settle")
	network := fs.String("network", "testnet", "Stellar CLI network name")
	source := fs.String("source", "operator", "Stellar CLI identity that submits")
	asJSON := fs.Bool("json", false, "print the batches as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 {
		return errors.New("--window is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if len(cfg.Contract) != 56 || cfg.Contract[0] != 'C' {
		return fmt.Errorf("%s: contract must be the settlement contract's C… address", *path)
	}
	w, err := clearing.ReadWindow(cfg.DataDir, *window)
	if err != nil {
		return err
	}
	batches, err := soroban.Plan(w, soroban.Mapping{Addresses: cfg.Addresses(), Tokens: cfg.Tokens})
	if errors.Is(err, soroban.ErrNothingToSettle) {
		_, err = fmt.Fprintf(out, "# window %d nets to zero: nothing to settle on chain\n", *window)
		return err
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"contract": cfg.Contract, "window": *window, "batches": batches})
	}
	invoke := fmt.Sprintf("stellar contract invoke --id %s --source %s --network %s --", cfg.Contract, *source, *network)
	fmt.Fprintf(out, "# window %d: %d submit call(s), then settle\n", *window, len(batches))
	for _, b := range batches {
		arg, err := json.Marshal(b)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s submit --obligations '%s'\n", invoke, arg)
	}
	_, err = fmt.Fprintf(out, "%s settle\n", invoke)
	return err
}
