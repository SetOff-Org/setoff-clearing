// Package clearing collects obligations into settlement windows and nets them.
//
// The open window is journaled to disk (fsync per obligation) so a restart
// loses nothing. Closing a window writes its final netting and starts a new one.
package clearing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

// Errors callers map to responses.
var (
	ErrInvalid       = errors.New("invalid obligation")
	ErrDuplicate     = errors.New("duplicate reference")
	ErrUnknownWindow = errors.New("unknown window")
)

// Closed is a settled window.
type Closed struct {
	Window      uint64               `json:"window"`
	ClosedAt    time.Time            `json:"closed_at"`
	Obligations []netting.Obligation `json:"obligations"`
	Netting     *netting.Result      `json:"netting"`
}

// Clearing is the window store.
type Clearing struct {
	mu          sync.Mutex
	dir         string
	window      uint64
	obligations []netting.Obligation
	ids         map[string]bool
	members     map[string]bool
	gross       map[string]*big.Int // per asset, for O(1) overflow checks
	assets      map[string]bool     // nil: any asset
}

// RestrictAssets limits new obligations to the given assets, so a mistyped
// code cannot open a book of its own that never nets against the real one.
// Obligations already in the open window are kept.
func (c *Clearing) RestrictAssets(assets []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.assets = map[string]bool{}
	for _, a := range assets {
		c.assets[a] = true
	}
}

// admit records an obligation already known to be valid.
func (c *Clearing) admit(o netting.Obligation) {
	c.obligations = append(c.obligations, o)
	c.ids[o.ID] = true
	if c.gross[o.Asset] == nil {
		c.gross[o.Asset] = new(big.Int)
	}
	c.gross[o.Asset].Add(c.gross[o.Asset], &o.Amount.Int)
}

// Open loads (or creates) the store in dir. members lists every participant id.
func Open(dir string, members []string) (*Clearing, error) {
	if err := os.MkdirAll(filepath.Join(dir, "windows"), 0o750); err != nil {
		return nil, err
	}
	c := &Clearing{dir: dir, ids: map[string]bool{}, members: map[string]bool{}, gross: map[string]*big.Int{}}
	for _, m := range members {
		c.members[m] = true
	}
	// The open window follows the newest archived one. Each window has its own
	// journal, so one left behind by a crash during Close belongs to an
	// archived window: it is discarded, never replayed into the next window.
	archived, err := numbered(filepath.Join(dir, "windows"), "", ".json")
	if err != nil {
		return nil, err
	}
	for _, n := range archived {
		c.window = max(c.window, n)
	}
	c.window++
	journals, err := numbered(dir, "open-", ".jsonl")
	if err != nil {
		return nil, err
	}
	for _, n := range journals {
		if n < c.window {
			if err := os.Remove(c.journalFor(n)); err != nil {
				return nil, err
			}
		}
	}
	data, err := os.ReadFile(c.journal())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	// Bytes after the last newline are a write torn by a crash. Obligations
	// are acknowledged only after their whole line is fsynced, so that one
	// never was: drop it, so the next append starts on a fresh line.
	complete := bytes.LastIndexByte(data, '\n') + 1
	if complete < len(data) {
		if err := os.Truncate(c.journal(), int64(complete)); err != nil {
			return nil, err
		}
	}
	for i, line := range bytes.Split(data[:complete], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var o netting.Obligation
		if err := json.Unmarshal(line, &o); err != nil {
			return nil, fmt.Errorf("journal line %d: %w", i+1, err)
		}
		c.admit(o)
	}
	return c, nil
}

func (c *Clearing) journal() string { return c.journalFor(c.window) }

func (c *Clearing) journalFor(n uint64) string {
	return filepath.Join(c.dir, fmt.Sprintf("open-%06d.jsonl", n))
}

// numbered lists the numbers n of files in dir named prefix + n + suffix.
func numbered(dir, prefix, suffix string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		digits, ok := strings.CutSuffix(name, suffix)
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(digits, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// Submission is what a participant sends: it can only commit itself to pay.
type Submission struct {
	Reference string         `json:"reference"`
	Creditor  string         `json:"creditor"`
	Asset     string         `json:"asset"`
	Amount    netting.Amount `json:"amount"`
}

// Submit records an obligation owed by debtor. Returns its id and window.
func (c *Clearing) Submit(debtor string, s Submission) (string, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Reference == "" || len(s.Reference) > 64 {
		return "", 0, fmt.Errorf("%w: reference must be 1-64 characters", ErrInvalid)
	}
	if !c.members[s.Creditor] {
		return "", 0, fmt.Errorf("%w: %q is not a participant", ErrInvalid, s.Creditor)
	}
	if c.assets != nil && !c.assets[s.Asset] {
		return "", 0, fmt.Errorf("%w: asset %q is not cleared here", ErrInvalid, s.Asset)
	}
	o := netting.Obligation{ID: debtor + ":" + s.Reference, Debtor: debtor, Creditor: s.Creditor, Asset: s.Asset, Amount: s.Amount}
	if c.ids[o.ID] {
		return "", 0, ErrDuplicate
	}
	if err := netting.Validate(o); err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// Bounding the asset's gross bounds every total the window can produce,
	// so Close can never overflow.
	if total := new(big.Int).Add(c.grossOf(o.Asset), &o.Amount.Int); !netting.InRange(total) {
		return "", 0, fmt.Errorf("%w: the window's %s total would overflow", ErrInvalid, o.Asset)
	}
	line, err := json.Marshal(o)
	if err != nil {
		return "", 0, err
	}
	f, err := os.OpenFile(c.journal(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	c.admit(o)
	return o.ID, c.window, nil
}

func (c *Clearing) grossOf(asset string) *big.Int {
	if g := c.gross[asset]; g != nil {
		return g
	}
	return new(big.Int)
}

// Preview nets the open window without closing it.
func (c *Clearing) Preview() (uint64, int, *netting.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := netting.Net(c.obligations)
	return c.window, len(c.obligations), res, err
}

// Close nets the open window, archives it and opens the next.
func (c *Clearing) Close(now time.Time) (*Closed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := netting.Net(c.obligations)
	if err != nil {
		return nil, err
	}
	obs := append([]netting.Obligation{}, c.obligations...)
	sort.Slice(obs, func(i, j int) bool { return obs[i].ID < obs[j].ID })
	closed := &Closed{Window: c.window, ClosedAt: now.UTC(), Obligations: obs, Netting: res}
	b, err := json.MarshalIndent(closed, "", "  ")
	if err != nil {
		return nil, err
	}
	path := c.windowPath(c.window)
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	if err := os.Remove(c.journal()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c.window++
	c.obligations, c.ids, c.gross = nil, map[string]bool{}, map[string]*big.Int{}
	return closed, nil
}

// Window returns an archived window.
func (c *Clearing) Window(n uint64) (*Closed, error) {
	b, err := os.ReadFile(c.windowPath(n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnknownWindow
	}
	if err != nil {
		return nil, err
	}
	var w Closed
	return &w, json.Unmarshal(b, &w)
}

func (c *Clearing) windowPath(n uint64) string {
	return filepath.Join(c.dir, "windows", fmt.Sprintf("%06d.json", n))
}

// View is one participant's slice of the open window.
type View struct {
	Window      uint64               `json:"window"`
	Obligations []netting.Obligation `json:"obligations"` // owed by or to the participant
	Positions   []netting.Position   `json:"positions"`   // the participant's net per asset
	Transfers   []netting.Transfer   `json:"transfers"`   // plan legs the participant pays or receives
}

// Mine returns what the open window means for one participant.
func (c *Clearing) Mine(participant string) (*View, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := netting.Net(c.obligations)
	if err != nil {
		return nil, err
	}
	v := &View{Window: c.window, Obligations: []netting.Obligation{}, Positions: []netting.Position{}, Transfers: []netting.Transfer{}}
	for _, o := range c.obligations {
		if o.Debtor == participant || o.Creditor == participant {
			v.Obligations = append(v.Obligations, o)
		}
	}
	sort.Slice(v.Obligations, func(i, j int) bool { return v.Obligations[i].ID < v.Obligations[j].ID })
	for _, p := range res.Positions {
		if p.Participant == participant {
			v.Positions = append(v.Positions, p)
		}
	}
	for _, t := range res.Transfers {
		if t.From == participant || t.To == participant {
			v.Transfers = append(v.Transfers, t)
		}
	}
	return v, nil
}

// Summary describes an archived window.
type Summary struct {
	Window      uint64    `json:"window"`
	ClosedAt    time.Time `json:"closed_at"`
	Obligations int       `json:"obligations"`
	Transfers   int       `json:"transfers"`
}

// Windows lists up to limit archived windows numbered below before (0: no
// bound), newest first.
func (c *Clearing) Windows(before uint64, limit int) ([]Summary, error) {
	numbers, err := numbered(filepath.Join(c.dir, "windows"), "", ".json")
	if err != nil {
		return nil, err
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] > numbers[j] })
	out := []Summary{}
	for _, n := range numbers {
		if len(out) == limit {
			break
		}
		if before != 0 && n >= before {
			continue
		}
		w, err := c.Window(n)
		if err != nil {
			return nil, err
		}
		out = append(out, Summary{Window: w.Window, ClosedAt: w.ClosedAt, Obligations: len(w.Obligations), Transfers: len(w.Netting.Transfers)})
	}
	return out, nil
}
