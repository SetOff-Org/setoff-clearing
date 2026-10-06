// Command setoff nets obligations and runs the SetOff clearing service.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/api"
	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/config"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

var version = "dev"

const usage = `setoff nets obligations between participants and runs a clearing service.

Usage:
  setoff net [--json] <obligations.csv|obligations.json|->
  setoff serve [--config setoff.toml]
  setoff soroban [--config setoff.toml] --window N [--network testnet] [--source operator] [--json]
  setoff version

CSV input needs the header: id,debtor,creditor,asset,amount

soroban prints the Stellar CLI calls that settle a closed window through the
SetOff settlement contract. Each submit must be authorized by its debtors.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "net":
		err = runNet(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "soroban":
		err = runSoroban(os.Args[2:], os.Stdout)
	case "version":
		fmt.Println("setoff", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func readObligations(path string) ([]netting.Obligation, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // G304: the user names the file to net
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(string(data)); strings.HasPrefix(trimmed, "[") {
		var obs []netting.Obligation
		return obs, json.Unmarshal(data, &obs)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || strings.Join(rows[0], ",") != "id,debtor,creditor,asset,amount" {
		return nil, errors.New("CSV header must be id,debtor,creditor,asset,amount")
	}
	obs := make([]netting.Obligation, 0, len(rows)-1)
	for i, row := range rows[1:] {
		amt, err := netting.ParseAmount(row[4])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+2, err)
		}
		obs = append(obs, netting.Obligation{ID: row[0], Debtor: row[1], Creditor: row[2], Asset: row[3], Amount: amt})
	}
	return obs, nil
}

func runNet(args []string) error {
	fs := flag.NewFlagSet("net", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the netting as JSON")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: setoff net [--json] <file|->")
	}
	obs, err := readObligations(fs.Arg(0))
	if err != nil {
		return err
	}
	res, err := netting.Net(obs)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ASSET\tOBLIGATIONS\tGROSS\tSETTLED\tSAVED\tTRANSFERS")
	for _, a := range res.Assets {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s%%\t%d\n", a.Asset, a.Obligations, a.Gross.String(), a.Settled.String(), pct(a.SavingBPS()), a.Transfers)
	}
	fmt.Fprintln(w, "\nPLAN\t\t\t\t\t")
	for _, t := range res.Transfers {
		fmt.Fprintf(w, "%s\t%s → %s\t%s\t\t\t\n", t.Asset, t.From, t.To, t.Amount.String())
	}
	return w.Flush()
}

func pct(bps int64) string {
	return new(big.Rat).SetFrac64(bps, 100).FloatString(2)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "setoff.toml", "config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	keys, ids, op, err := cfg.Keys()
	if err != nil {
		return err
	}
	store, err := clearing.Open(cfg.DataDir, ids)
	if err != nil {
		return err
	}
	if len(cfg.Assets) > 0 {
		store.RestrictAssets(cfg.Assets)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	s := &api.Server{Clearing: store, Participants: keys, OperatorKey: op, Log: logger, Now: time.Now, Decimals: cfg.Decimals}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("serving", "addr", cfg.Listen, "participants", len(ids))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
