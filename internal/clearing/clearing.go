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
}

// Open loads (or creates) the store in dir. members lists every participant id.
func Open(dir string, members []string) (*Clearing, error) {
	if err := os.MkdirAll(filepath.Join(dir, "windows"), 0o750); err != nil {
		return nil, err
	}
	c := &Clearing{dir: dir, ids: map[string]bool{}, members: map[string]bool{}}
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
		c.obligations = append(c.obligations, o)
		c.ids[o.ID] = true
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
	o := netting.Obligation{ID: debtor + ":" + s.Reference, Debtor: debtor, Creditor: s.Creditor, Asset: s.Asset, Amount: s.Amount}
	if c.ids[o.ID] {
		return "", 0, ErrDuplicate
	}
	// Validate against the whole window so totals can never overflow at close.
	if _, err := netting.Net(append(append([]netting.Obligation{}, c.obligations...), o)); err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrInvalid, err)
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
	c.obligations = append(c.obligations, o)
	c.ids[o.ID] = true
	return o.ID, c.window, nil
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
	c.obligations, c.ids = nil, map[string]bool{}
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
