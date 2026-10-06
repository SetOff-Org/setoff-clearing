package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics counts requests by route and status, in Prometheus text format.
type Metrics struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // route, status
	seconds  map[string]float64   // route -> total handler time
}

func (m *Metrics) observe(route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests, m.seconds = map[[2]string]uint64{}, map[string]float64{}
	}
	m.requests[[2]string{route, fmt.Sprint(status)}]++
	m.seconds[route] += d.Seconds()
}

// ServeHTTP writes the counters.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP setoff_clearing_requests_total Requests by route and HTTP status.\n")
	b.WriteString("# TYPE setoff_clearing_requests_total counter\n")
	keys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		fmt.Fprintf(&b, "setoff_clearing_requests_total{route=%q,status=%q} %d\n", k[0], k[1], m.requests[k])
	}
	b.WriteString("# HELP setoff_clearing_request_seconds_total Time spent handling requests.\n")
	b.WriteString("# TYPE setoff_clearing_request_seconds_total counter\n")
	routes := make([]string, 0, len(m.seconds))
	for r := range m.seconds {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		fmt.Fprintf(&b, "setoff_clearing_request_seconds_total{route=%q} %g\n", r, m.seconds[r])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// observe tags each request with an X-Request-ID, logs it and counts it.
//
// Requests are counted by the route pattern they matched (e.g.
// "/v1/windows/{n}"), so label cardinality stays bounded.
func (s *Server) observe(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		route := "other"
		if _, pattern := next.Handler(r); pattern != "" {
			_, route, _ = strings.Cut(pattern, " ") // drop the method
		}
		s.Metrics.observe(route, rec.status, elapsed)
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			s.Log.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("id", id), slog.String("method", r.Method), slog.String("path", r.URL.Path),
				slog.Int("status", rec.status), slog.Duration("elapsed", elapsed))
		}
	})
}
