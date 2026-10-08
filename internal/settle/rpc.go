package settle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// RPC is what settlement needs from a Stellar RPC server.
type RPC interface {
	Simulate(ctx context.Context, tx string) (*Simulation, error)
	Send(ctx context.Context, tx, hash string) (*Outcome, error)
	LatestLedger(ctx context.Context) (uint32, error)
	Sequence(ctx context.Context, account string) (int64, error)
}

// Simulation is the part of simulateTransaction's result settlement uses.
type Simulation struct {
	TransactionData string   // base64 SorobanTransactionData
	MinResourceFee  int64    // stroops
	Auth            []string // base64 SorobanAuthorizationEntry, as recorded
	Error           string
}

// Outcome is a submitted transaction's final status.
type Outcome struct {
	Status string // SUCCESS or FAILED
	Ledger uint32
}

// Client is a JSON-RPC client for a Stellar RPC server.
type Client struct {
	URL  string
	HTTP *http.Client
	Poll time.Duration
}

// NewClient returns a client for the RPC server at url.
func NewClient(url string) *Client {
	return &Client{URL: url, HTTP: &http.Client{Timeout: 30 * time.Second}, Poll: time.Second}
}

// Simulate runs simulateTransaction.
func (c *Client) Simulate(ctx context.Context, tx string) (*Simulation, error) {
	var out struct {
		TransactionData string `json:"transactionData"`
		MinResourceFee  string `json:"minResourceFee"`
		Error           string `json:"error"`
		Results         []struct {
			Auth []string `json:"auth"`
		} `json:"results"`
	}
	if err := c.call(ctx, "simulateTransaction", map[string]string{"transaction": tx}, &out); err != nil {
		return nil, err
	}
	sim := &Simulation{TransactionData: out.TransactionData, Error: out.Error}
	if out.Error == "" {
		fee, err := strconv.ParseInt(out.MinResourceFee, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("simulateTransaction: minResourceFee %q: %w", out.MinResourceFee, err)
		}
		sim.MinResourceFee = fee
	}
	if len(out.Results) > 0 {
		sim.Auth = out.Results[0].Auth
	}
	return sim, nil
}

// Send submits tx and waits for it to be applied.
func (c *Client) Send(ctx context.Context, tx, hash string) (*Outcome, error) {
	var sent struct {
		Status         string `json:"status"`
		ErrorResultXDR string `json:"errorResultXdr"`
	}
	for attempt := 0; ; attempt++ {
		if err := c.call(ctx, "sendTransaction", map[string]string{"transaction": tx}, &sent); err != nil {
			return nil, err
		}
		if sent.Status != "TRY_AGAIN_LATER" || attempt == 10 {
			break
		}
		if err := sleep(ctx, c.Poll); err != nil {
			return nil, err
		}
	}
	if sent.Status != "PENDING" && sent.Status != "DUPLICATE" {
		return nil, fmt.Errorf("sendTransaction: %s %s", sent.Status, sent.ErrorResultXDR)
	}
	for {
		var got struct {
			Status string `json:"status"`
			Ledger uint32 `json:"ledger"`
		}
		if err := c.call(ctx, "getTransaction", map[string]string{"hash": hash}, &got); err != nil {
			return nil, err
		}
		if got.Status == "SUCCESS" || got.Status == "FAILED" {
			return &Outcome{Status: got.Status, Ledger: got.Ledger}, nil
		}
		if err := sleep(ctx, c.Poll); err != nil {
			return nil, fmt.Errorf("waiting for %s: %w", hash, err)
		}
	}
}

// LatestLedger runs getLatestLedger.
func (c *Client) LatestLedger(ctx context.Context) (uint32, error) {
	var out struct {
		Sequence uint32 `json:"sequence"`
	}
	if err := c.call(ctx, "getLatestLedger", nil, &out); err != nil {
		return 0, err
	}
	return out.Sequence, nil
}

// Sequence reads an account's current sequence number.
func (c *Client) Sequence(ctx context.Context, account string) (int64, error) {
	id, err := xdr.AddressToAccountId(account)
	if err != nil {
		return 0, err
	}
	key, err := xdr.MarshalBase64(xdr.LedgerKey{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.LedgerKeyAccount{AccountId: id}})
	if err != nil {
		return 0, err
	}
	var out struct {
		Entries []struct {
			XDR string `json:"xdr"`
		} `json:"entries"`
	}
	if err := c.call(ctx, "getLedgerEntries", map[string][]string{"keys": {key}}, &out); err != nil {
		return 0, err
	}
	if len(out.Entries) == 0 {
		return 0, fmt.Errorf("account %s does not exist", account)
	}
	var data xdr.LedgerEntryData
	if err := xdr.SafeUnmarshalBase64(out.Entries[0].XDR, &data); err != nil || data.Account == nil {
		return 0, fmt.Errorf("getLedgerEntries: unreadable account entry for %s", account)
	}
	return int64(data.Account.SeqNum), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "setoff-clearing")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %s", method, env.Error.Message)
	}
	return json.Unmarshal(env.Result, out)
}
