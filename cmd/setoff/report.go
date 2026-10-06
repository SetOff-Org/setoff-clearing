package main

import (
	"errors"
	"flag"
	"io"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/config"
	"github.com/SetOff-Org/setoff-clearing/internal/report"
)

func runReport(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	path := fs.String("config", "setoff.toml", "config file")
	window := fs.Uint64("window", 0, "closed window to report")
	participant := fs.String("participant", "", "only this participant's statements")
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
	w, err := clearing.ReadWindow(cfg.DataDir, *window)
	if err != nil {
		return err
	}
	xml, err := report.Camt053(w, report.Options{Decimals: cfg.Decimals, Participant: *participant, Now: time.Now()})
	if err != nil {
		return err
	}
	_, err = out.Write(xml)
	return err
}
