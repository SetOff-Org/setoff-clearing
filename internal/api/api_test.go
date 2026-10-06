package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/api"
	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
)

var keys = map[string]string{"key-a": "anchor-a", "key-b": "anchor-b", "key-c": "anchor-c"}

func server(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	c, err := clearing.Open(dir, []string{"anchor-a", "anchor-b", "anchor-c"})
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{
		Clearing: c, Participants: keys, OperatorKey: "op",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) },
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func owe(creditor, ref, amount string) map[string]string {
	return map[string]string{"reference": ref, "creditor": creditor, "asset": "USDC", "amount": amount}
}

func TestACycleNetsToNothingAndArchives(t *testing.T) {
	srv := server(t, t.TempDir())
	for key, ob := range map[string]map[string]string{
		"key-a": owe("anchor-b", "inv-1", "500"),
		"key-b": owe("anchor-c", "inv-2", "500"),
		"key-c": owe("anchor-a", "inv-3", "500"),
	} {
		if code, out := call(t, srv, "POST", "/v1/obligations", key, ob); code != http.StatusCreated {
			t.Fatalf("submit: %d %v", code, out)
		}
	}
	code, preview := call(t, srv, "GET", "/v1/window", "key-a", nil)
	netting := preview["netting"].(map[string]any)
	if code != 200 || preview["obligations"].(float64) != 3 || len(netting["transfers"].([]any)) != 0 {
		t.Fatalf("preview: %d %v", code, preview)
	}

	code, closed := call(t, srv, "POST", "/v1/window/close", "op", nil)
	if code != 200 || closed["window"].(float64) != 1 {
		t.Fatalf("close: %d %v", code, closed)
	}
	asset := closed["netting"].(map[string]any)["assets"].([]any)[0].(map[string]any)
	if asset["gross"] != "1500" || asset["settled"] != "0" {
		t.Fatalf("totals: %v", asset)
	}
	if code, _ := call(t, srv, "GET", "/v1/windows/1", "key-b", nil); code != 200 {
		t.Fatal("archived window not served")
	}
	if _, next := call(t, srv, "GET", "/v1/window", "op", nil); next["window"].(float64) != 2 || next["obligations"].(float64) != 0 {
		t.Fatalf("next window not clean: %v", next)
	}
}

func TestTheOpenWindowSurvivesRestarts(t *testing.T) {
	dir := t.TempDir()
	srv := server(t, dir)
	call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "100"))
	srv.Close()
	srv = server(t, dir)
	if _, p := call(t, srv, "GET", "/v1/window", "op", nil); p["obligations"].(float64) != 1 {
		t.Fatalf("journal not replayed: %v", p)
	}
	if code, _ := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "100")); code != http.StatusConflict {
		t.Fatalf("duplicate after restart: %d", code)
	}
}

func TestParticipantsCanOnlyCommitThemselves(t *testing.T) {
	srv := server(t, t.TempDir())
	// The debtor is always the authenticated participant; there is no field to override it.
	body := map[string]string{"reference": "x", "creditor": "anchor-b", "asset": "USDC", "amount": "1", "debtor": "anchor-c"}
	if code, _ := call(t, srv, "POST", "/v1/obligations", "key-a", body); code != http.StatusBadRequest {
		t.Fatalf("debtor override accepted: %d", code)
	}
}

func TestBadRequests(t *testing.T) {
	srv := server(t, t.TempDir())
	cases := map[string]struct {
		key  string
		body map[string]string
		want int
	}{
		"no key":           {"", owe("anchor-b", "1", "5"), http.StatusUnauthorized},
		"operator submits": {"op", owe("anchor-b", "1", "5"), http.StatusUnauthorized},
		"unknown creditor": {"key-a", owe("mallory", "1", "5"), http.StatusBadRequest},
		"self":             {"key-a", owe("anchor-a", "1", "5"), http.StatusBadRequest},
		"zero":             {"key-a", owe("anchor-b", "1", "0"), http.StatusBadRequest},
		"not canonical":    {"key-a", owe("anchor-b", "1", "05"), http.StatusBadRequest},
		"no reference":     {"key-a", owe("anchor-b", "", "5"), http.StatusBadRequest},
	}
	for name, c := range cases {
		if code, out := call(t, srv, "POST", "/v1/obligations", c.key, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", name, code, out, c.want)
		}
	}
	if code, _ := call(t, srv, "POST", "/v1/window/close", "key-a", nil); code != http.StatusUnauthorized {
		t.Error("participant closed a window")
	}
	if code, _ := call(t, srv, "GET", "/v1/windows/9", "op", nil); code != http.StatusNotFound {
		t.Error("unknown window served")
	}
}

func TestMeIsScopedToTheCaller(t *testing.T) {
	srv := server(t, t.TempDir())
	call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "1", "100"))
	call(t, srv, "POST", "/v1/obligations", "key-b", owe("anchor-c", "1", "60"))

	code, me := call(t, srv, "GET", "/v1/me", "key-c", nil)
	if code != http.StatusOK || len(me["obligations"].([]any)) != 1 {
		t.Fatalf("status %d: %v", code, me)
	}
	if pos := me["positions"].([]any); len(pos) != 1 || pos[0].(map[string]any)["net"] != "60" {
		t.Fatalf("%v", me)
	}
	for _, key := range []string{"", "op"} {
		if code, _ := call(t, srv, "GET", "/v1/me", key, nil); code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d", key, code)
		}
	}
}
