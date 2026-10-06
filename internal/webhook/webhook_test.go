package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var secret = []byte("whsec-test")

func TestDeliveriesAreSignedAndVerifiable(t *testing.T) {
	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- Verify(secret, r.Header.Get("X-SetOff-Timestamp"), r.Header.Get("X-SetOff-Signature"), body, time.Now(), 5*time.Minute)
	}))
	defer srv.Close()
	if err := New(srv.URL, secret).Send(context.Background(), map[string]any{"type": "window.closed", "window": 7}); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatalf("receiver rejected a genuine delivery: %v", err)
	}
}

func TestVerifyRejectsForgeriesAndReplays(t *testing.T) {
	now := time.Unix(1_791_300_000, 0)
	body := []byte(`{"window":7}`)
	sig := Sign(secret, now.Unix(), body)
	ts := "1791300000"
	for name, c := range map[string]struct {
		secret []byte
		ts     string
		body   string
		at     time.Time
	}{
		"wrong secret": {[]byte("other"), ts, string(body), now},
		"edited body":  {secret, ts, `{"window":8}`, now},
		"replayed":     {secret, ts, string(body), now.Add(time.Hour)},
		"bad header":   {secret, "soon", string(body), now},
	} {
		if err := Verify(c.secret, c.ts, sig, []byte(c.body), c.at, 5*time.Minute); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Verify(secret, ts, sig, body, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestFailuresAreRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	n := New(srv.URL, secret)
	n.Backoff = time.Millisecond
	if err := n.Send(context.Background(), "x"); err != nil || calls.Load() != 3 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}

	calls.Store(-100) // fail every time
	n.Retries = 2
	if err := n.Send(context.Background(), "x"); err == nil || calls.Load() != -97 {
		t.Fatalf("want failure after 3 attempts, got %v after %d", err, calls.Load()+100)
	}
}
