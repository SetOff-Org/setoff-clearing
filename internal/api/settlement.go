package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/SetOff-Org/setoff-clearing/internal/settle"
	"github.com/SetOff-Org/setoff-clearing/internal/soroban"
)

// Settlement settles closed windows on chain, when configured.
type Settlement struct {
	Settler *settle.Settler
	Mapping soroban.Mapping
	// Background is the context submissions run under once a request has
	// returned; cancelling it stops them.
	Background context.Context
	// Done, if set, is told the outcome of every background Advance.
	Done func(*settle.Session, error)
}

// authorization is one entry a participant must sign.
type authorization struct {
	Batch            int    `json:"batch"`
	Slot             int    `json:"slot"`
	Entry            string `json:"entry"`
	Hash             string `json:"hash"`
	ExpirationLedger uint32 `json:"expiration_ledger"`
	Signed           bool   `json:"signed"`
}

func (s *Server) settlementRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/windows/{n}/settlement", s.prepare)
	mux.HandleFunc("GET /v1/windows/{n}/settlement", s.anyone(s.settlement))
	mux.HandleFunc("GET /v1/windows/{n}/authorizations", s.authorizations)
	mux.HandleFunc("POST /v1/windows/{n}/authorizations", s.authorize)
}

// prepare is the operator's POST /v1/windows/{n}/settlement.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	if !s.operator(r) {
		reply(w, http.StatusUnauthorized, errBody("operator key required"))
		return
	}
	if !s.settles(w) {
		return
	}
	closed, ok := s.lookup(w, r)
	if !ok {
		return
	}
	sess, err := s.Settlement.Settler.Prepare(r.Context(), closed, s.Settlement.Mapping)
	if errors.Is(err, soroban.ErrNothingToSettle) {
		reply(w, http.StatusConflict, errBody(err.Error()))
		return
	}
	if err != nil {
		s.Log.Error("prepare settlement", "window", closed.Window, "err", err)
		reply(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	reply(w, http.StatusOK, sess)
}

// settlement is GET /v1/windows/{n}/settlement: the session's progress.
func (s *Server) settlement(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	reply(w, http.StatusOK, map[string]any{"session": sess, "pending": sess.Pending(), "settled": sess.Settled()})
}

// authorizations is GET /v1/windows/{n}/authorizations: the caller's entries to sign.
func (s *Server) authorizations(w http.ResponseWriter, r *http.Request) {
	address, ok := s.debtor(w, r)
	if !ok {
		return
	}
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	out := []authorization{}
	for i, b := range sess.Batches {
		if b.Tx != nil && b.Tx.Status == "SUCCESS" {
			continue
		}
		for _, e := range b.Entries {
			if e.Address == address {
				out = append(out, authorization{Batch: i, Slot: e.Slot, Entry: e.Entry, Hash: e.Hash, ExpirationLedger: e.ExpirationLedger, Signed: e.Signed != ""})
			}
		}
	}
	reply(w, http.StatusOK, map[string]any{"window": sess.Window, "address": address, "authorizations": out})
}

// authorize is POST /v1/windows/{n}/authorizations: signed entries back.
// Once the last one for a batch arrives, submission starts in the background.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	address, ok := s.debtor(w, r)
	if !ok {
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil {
		reply(w, http.StatusBadRequest, errBody("window must be a number"))
		return
	}
	var req struct {
		Authorizations []settle.Signature `json:"authorizations"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Authorizations) == 0 {
		reply(w, http.StatusBadRequest, errBody(`body must be {"authorizations": [{"batch", "slot", "entry" or "signature"}]}`))
		return
	}
	sess, err := s.Settlement.Settler.Accept(n, address, req.Authorizations)
	switch {
	case errors.Is(err, settle.ErrNotPrepared):
		reply(w, http.StatusNotFound, errBody(err.Error()))
		return
	case errors.Is(err, settle.ErrInvalid):
		reply(w, http.StatusUnprocessableEntity, errBody(err.Error()))
		return
	case err != nil:
		s.Log.Error("accept authorizations", "window", n, "err", err)
		reply(w, http.StatusInternalServerError, errBody("could not record authorizations"))
		return
	}
	go s.advance(n)
	reply(w, http.StatusAccepted, map[string]any{"window": n, "accepted": len(req.Authorizations), "pending": sess.Pending()})
}

func (s *Server) advance(n uint64) {
	ctx := s.Settlement.Background
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := s.Settlement.Settler.Advance(ctx, n)
	if err != nil {
		s.Log.Error("settlement", "window", n, "err", err)
	} else if sess.Settled() {
		s.Log.Info("settled on chain", "window", n, "settle_tx", sess.Settle.Hash)
	}
	if s.Settlement.Done != nil {
		s.Settlement.Done(sess, err)
	}
}

// debtor returns the calling participant's Stellar address.
func (s *Server) debtor(w http.ResponseWriter, r *http.Request) (string, bool) {
	who, ok := s.participant(r)
	if !ok {
		reply(w, http.StatusUnauthorized, errBody("participant key required"))
		return "", false
	}
	if !s.settles(w) {
		return "", false
	}
	address := s.Settlement.Mapping.Addresses[who]
	if address == "" {
		reply(w, http.StatusConflict, errBody("participant "+who+" has no Stellar address configured"))
		return "", false
	}
	return address, true
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) (*settle.Session, bool) {
	if !s.settles(w) {
		return nil, false
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil {
		reply(w, http.StatusBadRequest, errBody("window must be a number"))
		return nil, false
	}
	sess, err := s.Settlement.Settler.Session(n)
	if errors.Is(err, settle.ErrNotPrepared) {
		reply(w, http.StatusNotFound, errBody(err.Error()))
		return nil, false
	}
	if err != nil {
		reply(w, http.StatusInternalServerError, errBody(err.Error()))
		return nil, false
	}
	return sess, true
}

func (s *Server) settles(w http.ResponseWriter) bool {
	if s.Settlement == nil {
		reply(w, http.StatusNotFound, errBody("on-chain settlement is not configured"))
		return false
	}
	return true
}
