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
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/SetOff-Org/setoff-clearing/internal/api"
	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/config"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
	"github.com/SetOff-Org/setoff-clearing/internal/sep10"
	"github.com/SetOff-Org/setoff-clearing/internal/settle"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
	"github.com/SetOff-Org/setoff-clearing/internal/webhook"
)

var version = "dev"

const usage = `setoff nets obligations between participants and runs a clearing service.

Usage:
  setoff net [--json] <obligations.csv|obligations.json|->
  setoff serve [--config setoff.toml]
  setoff soroban [--config setoff.toml] --window N [--network testnet] [--source operator] [--json]
  setoff report [--config setoff.toml] --window N [--participant ID]   ISO 20022 camt.053
  setoff authorize --window N --key-env VAR --secret-env VAR [--url URL]
  setoff version

CSV input needs the header: id,debtor,creditor,asset,amount

soroban prints the Stellar CLI calls that settle a closed window through the
SetOff settlement contract. Each submit must be authorized by its debtors.

With [settlement] configured, serve does this itself: debtors fetch and sign
their authorization entries over the API, and the service submits each batch
and settles. authorize is that step for a participant holding a plain
account key.
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
	case "report":
		err = runReport(os.Args[2:], os.Stdout)
	case "authorize":
		err = runAuthorize(os.Args[2:], os.Stdout)
	case "version":
		fmt.Println("setoff", buildVersion())
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
	if cfg.SEP10.HomeDomain != "" {
		auth, err := sep10.New(os.Getenv(cfg.SEP10.SigningKeyEnv), cfg.SEP10.HomeDomain,
			sep10.Passphrase(cfg.SEP10.Network), []byte(os.Getenv(cfg.SEP10.TokenSecretEnv)))
		if err != nil {
			return fmt.Errorf("sep10: %w", err)
		}
		s.SEP10, s.Addresses = auth, map[string]string{}
		for id, address := range cfg.Addresses() {
			s.Addresses[address] = id
		}
		logger.Info("sep10 enabled", "home_domain", cfg.SEP10.HomeDomain, "signing_key", auth.SigningKey())
	}
	notify, err := notifier(cfg, logger)
	if err != nil {
		return err
	}
	if s.Settlement, err = settlement(ctx, cfg, logger); err != nil {
		return err
	}
	if s.Settlement != nil && cfg.Settlement.PrepareOnClose {
		notify = prepareOnClose(ctx, s.Settlement, logger, notify)
	}
	s.OnClose = notify
	if every := cfg.CloseEvery.Duration; every > 0 {
		go store.AutoClose(ctx, every, func(w *clearing.Closed, err error) {
			if err != nil {
				logger.Error("scheduled close", "err", err)
				return
			}
			logger.Info("closed window", "window", w.Window, "obligations", len(w.Obligations), "transfers", len(w.Netting.Transfers))
			notify(w)
		})
	}
	logger.Info("serving", "addr", cfg.Listen, "participants", len(ids), "close_every", cfg.CloseEvery.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// settlement builds on-chain settlement from [settlement], if configured.
func settlement(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*api.Settlement, error) {
	st := cfg.Settlement
	if st.RPC == "" {
		return nil, nil
	}
	operator, err := keypair.ParseFull(os.Getenv(st.OperatorSecretEnv))
	if err != nil {
		return nil, fmt.Errorf("settlement: $%s must hold the operator's S… secret key", st.OperatorSecretEnv)
	}
	logger.Info("on-chain settlement enabled", "contract", cfg.Contract, "operator", operator.Address(), "rpc", st.RPC)
	return &api.Settlement{
		Settler: &settle.Settler{
			RPC: settle.NewClient(st.RPC), Passphrase: sep10.Passphrase(st.Network), Contract: cfg.Contract,
			Operator: operator, Dir: filepath.Join(cfg.DataDir, "settlement"), Validity: st.ValidityLedgers,
		},
		Mapping:    soroban.Mapping{Addresses: cfg.Addresses(), Tokens: cfg.Tokens, PositionQuota: st.PositionQuota},
		Background: ctx,
	}, nil
}

// prepareOnClose wraps the close hook to prepare settlement of every closed window.
func prepareOnClose(ctx context.Context, st *api.Settlement, logger *slog.Logger, next func(*clearing.Closed)) func(*clearing.Closed) {
	return func(w *clearing.Closed) {
		next(w)
		go func() {
			sess, err := st.Settler.Prepare(ctx, w, st.Mapping)
			switch {
			case errors.Is(err, soroban.ErrNothingToSettle):
			case err != nil:
				logger.Error("prepare settlement", "window", w.Window, "err", err)
			default:
				logger.Info("settlement prepared", "window", w.Window, "batches", len(sess.Batches), "authorizations", sess.Pending())
			}
		}()
	}
}

// notifier returns the close hook: a signed webhook delivery when configured,
// otherwise nothing.
func notifier(cfg *config.Config, logger *slog.Logger) (func(*clearing.Closed), error) {
	if cfg.Webhook.URL == "" {
		return func(*clearing.Closed) {}, nil
	}
	secret := os.Getenv(cfg.Webhook.SecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("webhook: $%s is empty", cfg.Webhook.SecretEnv)
	}
	n := webhook.New(cfg.Webhook.URL, []byte(secret))
	return func(w *clearing.Closed) {
		event := map[string]any{
			"type": "window.closed", "window": w.Window, "closed_at": w.ClosedAt,
			"obligations": len(w.Obligations), "assets": w.Netting.Assets,
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := n.Send(ctx, event); err != nil {
				logger.Error("webhook", "window", w.Window, "err", err)
			}
		}()
	}, nil
}
