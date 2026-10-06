package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
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
	// References survive the restart: a retry replays, a clash conflicts.
	if code, _ := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "100")); code != http.StatusOK {
		t.Fatalf("retry after restart: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "99")); code != http.StatusConflict {
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

func TestWindowsAreListed(t *testing.T) {
	srv := server(t, t.TempDir())
	for i := range 2 {
		call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", fmt.Sprint(i), "5"))
		call(t, srv, "POST", "/v1/window/close", "op", nil)
	}
	code, out := call(t, srv, "GET", "/v1/windows?limit=1", "key-b", nil)
	list, _ := out["windows"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["window"].(float64) != 2 {
		t.Fatalf("status %d: %v", code, out)
	}
	for _, q := range []string{"?limit=0", "?limit=x", "?before=-1"} {
		if code, _ := call(t, srv, "GET", "/v1/windows"+q, "key-b", nil); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, code)
		}
	}
	if code, _ := call(t, srv, "GET", "/v1/windows", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no key: status %d", code)
	}
}

func TestCamt053IsScopedToTheCaller(t *testing.T) {
	srv := server(t, t.TempDir())
	call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "1", "100"))
	call(t, srv, "POST", "/v1/obligations", "key-c", owe("anchor-b", "1", "30"))
	call(t, srv, "POST", "/v1/window/close", "op", nil)

	get := func(key string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/windows/1/camt053", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, mine := get("key-a")
	if code != http.StatusOK || strings.Count(mine, "<Stmt>") != 1 || strings.Contains(mine, "anchor-c:1") {
		t.Fatalf("status %d:\n%s", code, mine)
	}
	if _, all := get("op"); strings.Count(all, "<Stmt>") != 3 {
		t.Fatalf("the operator sees every statement:\n%s", all)
	}
	if code, _ := call(t, srv, "GET", "/v1/windows/9/camt053", "op", nil); code != http.StatusNotFound {
		t.Fatalf("status %d", code)
	}
}

func TestRequestsAreTaggedAndCountedByRoute(t *testing.T) {
	srv := server(t, t.TempDir())
	for _, n := range []string{"7", "8"} {
		call(t, srv, "GET", "/v1/windows/"+n, "key-a", nil)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-ID", "trace-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("X-Request-ID") != "trace-1" {
		t.Fatal("request id not echoed")
	}
	resp, err = http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `setoff_clearing_requests_total{route="/v1/windows/{n}",status="404"} 2`) {
		t.Fatalf("windows 7 and 8 should share one label:\n%s", body)
	}
}

// Every route in Handler's doc comment must be described in api/openapi.yaml.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`(?m)^//\t(GET|POST) +(/\S+)`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 9 {
		t.Fatalf("found only %d routes in api.go", len(routes))
	}
	for _, r := range routes {
		method, path := strings.ToLower(r[1]), r[2]
		block := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(path) + `:\n((?:    .*\n|\n)*)`).FindStringSubmatch(string(spec))
		if block == nil || !strings.Contains(block[1], "    "+method+":") {
			t.Errorf("api/openapi.yaml does not describe %s %s", r[1], path)
		}
	}
}

func TestClosingNotifies(t *testing.T) {
	c, err := clearing.Open(t.TempDir(), []string{"anchor-a", "anchor-b", "anchor-c"})
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan uint64, 1)
	s := &api.Server{
		Clearing: c, Participants: keys, OperatorKey: "op",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		OnClose: func(w *clearing.Closed) { notified <- w.Window },
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "1", "5"))
	if code, _ := call(t, srv, "POST", "/v1/window/close", "op", nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if w := <-notified; w != 1 {
		t.Fatalf("notified about window %d", w)
	}
}

func TestRetriedObligationsReplay(t *testing.T) {
	srv := server(t, t.TempDir())
	code, first := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "10"))
	if code != http.StatusCreated {
		t.Fatal(code, first)
	}
	code, again := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "10"))
	if code != http.StatusOK || again["id"] != first["id"] || again["replayed"] != true {
		t.Fatalf("status %d: %v", code, again)
	}
	if code, _ := call(t, srv, "POST", "/v1/obligations", "key-a", owe("anchor-b", "r1", "11")); code != http.StatusConflict {
		t.Fatalf("status %d", code)
	}
}
