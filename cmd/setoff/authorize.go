package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// runAuthorize signs, as a participant, the authorization entries a clearing
// service issued for a window, with the participant's own account key.
func runAuthorize(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("authorize", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:7500", "clearing service")
	window := fs.Uint64("window", 0, "closed window")
	keyEnv := fs.String("key-env", "", "environment variable holding the participant's API key")
	secretEnv := fs.String("secret-env", "", "environment variable holding the participant's S… account secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 || *keyEnv == "" || *secretEnv == "" {
		return errors.New("--window, --key-env and --secret-env are required")
	}
	kp, err := keypair.ParseFull(os.Getenv(*secretEnv))
	if err != nil {
		return fmt.Errorf("$%s must hold an S… secret key", *secretEnv)
	}
	c := &serviceClient{url: strings.TrimRight(*url, "/"), key: os.Getenv(*keyEnv), http: &http.Client{Timeout: 30 * time.Second}}
	var issued struct {
		Address        string `json:"address"`
		Authorizations []struct {
			Batch            int    `json:"batch"`
			Slot             int    `json:"slot"`
			Hash             string `json:"hash"`
			ExpirationLedger uint32 `json:"expiration_ledger"`
			Signed           bool   `json:"signed"`
		} `json:"authorizations"`
	}
	path := fmt.Sprintf("/v1/windows/%d/authorizations", *window)
	if err := c.do(http.MethodGet, path, nil, &issued); err != nil {
		return err
	}
	if issued.Address != kp.Address() {
		return fmt.Errorf("the service knows this participant as %s, but $%s is the key of %s", issued.Address, *secretEnv, kp.Address())
	}
	var sigs []map[string]any
	for _, a := range issued.Authorizations {
		if a.Signed {
			continue
		}
		hash, err := hex.DecodeString(a.Hash)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("batch %d slot %d: bad hash from the service", a.Batch, a.Slot)
		}
		sig, err := kp.Sign(hash)
		if err != nil {
			return err
		}
		sigs = append(sigs, map[string]any{"batch": a.Batch, "slot": a.Slot, "signature": hex.EncodeToString(sig)})
		fmt.Fprintf(out, "signed batch %d slot %d (valid until ledger %d)\n", a.Batch, a.Slot, a.ExpirationLedger)
	}
	if len(sigs) == 0 {
		_, err := fmt.Fprintln(out, "nothing to sign")
		return err
	}
	var accepted struct {
		Pending int `json:"pending"`
	}
	if err := c.do(http.MethodPost, path, map[string]any{"authorizations": sigs}, &accepted); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "accepted; %d authorization(s) still pending from others\n", accepted.Pending)
	return err
}

type serviceClient struct {
	url, key string
	http     *http.Client
}

func (c *serviceClient) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.url+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
