// Package webhook tells other systems when a window closes.
//
// Each delivery is a JSON POST signed with HMAC-SHA256 over
// "<timestamp>.<body>", sent as
//
//	X-SetOff-Timestamp: <unix seconds>
//	X-SetOff-Signature: sha256=<hex>
//
// Receivers recompute the signature with the shared secret and reject stale
// timestamps, which stops both forgery and replay.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Notifier posts events to one URL.
type Notifier struct {
	URL     string
	Secret  []byte
	HTTP    *http.Client
	Retries int           // attempts after the first
	Backoff time.Duration // doubled after each failed attempt
	Now     func() time.Time
}

// New returns a notifier with a 10 s timeout and three retries.
func New(url string, secret []byte) *Notifier {
	return &Notifier{URL: url, Secret: secret, HTTP: &http.Client{Timeout: 10 * time.Second}, Retries: 3, Backoff: time.Second, Now: time.Now}
}

// Sign returns the signature header value for a timestamp and body.
func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a delivery's signature, refusing timestamps more than
// tolerance away from now. For receivers written in Go.
func Verify(secret []byte, timestampHeader, signatureHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp: %w", err)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("timestamp is %s away", d.Round(time.Second))
	}
	if !hmac.Equal([]byte(signatureHeader), []byte(Sign(secret, ts, body))) {
		return fmt.Errorf("signature does not match")
	}
	return nil
}

// Send delivers event, retrying failures (network errors and non-2xx
// responses) with exponential backoff until ctx ends.
func (n *Notifier) Send(ctx context.Context, event any) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	wait := n.Backoff
	for attempt := 0; ; attempt++ {
		err = n.post(ctx, body)
		if err == nil || attempt == n.Retries {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %w)", ctx.Err(), err)
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (n *Notifier) post(ctx context.Context, body []byte) error {
	ts := n.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "setoff-clearing")
	req.Header.Set("X-SetOff-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-SetOff-Signature", Sign(n.Secret, ts, body))
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}
