// Package api exposes the clearing service over HTTP.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
)

// Server serves one clearing store.
type Server struct {
	Clearing     *clearing.Clearing
	Participants map[string]string // API key -> participant id
	OperatorKey  string
	Log          *slog.Logger
	Now          func() time.Time
}

// Handler returns the routes.
//
//	POST /v1/obligations       participant: {"reference","creditor","asset","amount"}
//	GET  /v1/window            participant or operator: open window preview
//	GET  /v1/me                participant: own obligations, positions and plan legs
//	POST /v1/window/close      operator: net, archive, open the next window
//	GET  /v1/windows           participant or operator: archived windows, newest first (?before=n&limit=50)
//	GET  /v1/windows/{n}       participant or operator: an archived window
//	GET  /healthz
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/obligations", s.submit)
	mux.HandleFunc("GET /v1/window", s.anyone(s.preview))
	mux.HandleFunc("GET /v1/me", s.me)
	mux.HandleFunc("POST /v1/window/close", s.close)
	mux.HandleFunc("GET /v1/windows", s.anyone(s.windows))
	mux.HandleFunc("GET /v1/windows/{n}", s.anyone(s.archived))
	return mux
}

func bearer(r *http.Request) string {
	t, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return t
}

// participant returns the caller's participant id, comparing keys in constant time.
func (s *Server) participant(r *http.Request) (string, bool) {
	given := []byte(bearer(r))
	for key, id := range s.Participants {
		if subtle.ConstantTimeCompare(given, []byte(key)) == 1 {
			return id, true
		}
	}
	return "", false
}

func (s *Server) operator(r *http.Request) bool {
	return s.OperatorKey != "" && subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.OperatorKey)) == 1
}

func (s *Server) anyone(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.participant(r); !ok && !s.operator(r) {
			reply(w, http.StatusUnauthorized, errBody("missing or unknown key"))
			return
		}
		next(w, r)
	}
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	debtor, ok := s.participant(r)
	if !ok {
		reply(w, http.StatusUnauthorized, errBody("participant key required"))
		return
	}
	var sub clearing.Submission
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sub); err != nil {
		reply(w, http.StatusBadRequest, errBody("body must be {reference, creditor, asset, amount}: "+err.Error()))
		return
	}
	id, window, err := s.Clearing.Submit(debtor, sub)
	switch {
	case errors.Is(err, clearing.ErrDuplicate):
		reply(w, http.StatusConflict, errBody(err.Error()))
	case errors.Is(err, clearing.ErrInvalid):
		reply(w, http.StatusBadRequest, errBody(err.Error()))
	case err != nil:
		s.Log.Error("submit", "err", err)
		reply(w, http.StatusInternalServerError, errBody("could not record obligation"))
	default:
		reply(w, http.StatusCreated, map[string]any{"id": id, "window": window})
	}
}

func (s *Server) preview(w http.ResponseWriter, _ *http.Request) {
	window, n, res, err := s.Clearing.Preview()
	if err != nil {
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, map[string]any{"window": window, "obligations": n, "netting": res})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	who, ok := s.participant(r)
	if !ok {
		reply(w, http.StatusUnauthorized, errBody("participant key required"))
		return
	}
	v, err := s.Clearing.Mine(who)
	if err != nil {
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, v)
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	if !s.operator(r) {
		reply(w, http.StatusUnauthorized, errBody("operator key required"))
		return
	}
	closed, err := s.Clearing.Close(s.Now())
	if err != nil {
		s.Log.Error("close", "err", err)
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, closed)
}

func (s *Server) windows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, limit := uint64(0), 50
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			reply(w, http.StatusBadRequest, errBody("before must be a window number"))
			return
		}
		before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			reply(w, http.StatusBadRequest, errBody("limit must be 1-500"))
			return
		}
		limit = n
	}
	list, err := s.Clearing.Windows(before, limit)
	if err != nil {
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, map[string]any{"windows": list})
}

func (s *Server) archived(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil {
		reply(w, http.StatusBadRequest, errBody("window must be a number"))
		return
	}
	closed, err := s.Clearing.Window(n)
	if errors.Is(err, clearing.ErrUnknownWindow) {
		reply(w, http.StatusNotFound, errBody(err.Error()))
		return
	}
	if err != nil {
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, closed)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
