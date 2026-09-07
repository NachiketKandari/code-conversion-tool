// Package ledger records conversion state (PRD G8, §4.6): one status entry
// per plan unit (planned → generated → validated | failed → appended, plus
// blocked/skipped) so runs are resumable, and the source→target conversion
// map that answers "what happened to this function/query?" after the fact.
// State lives in conversion_logs/ledger/<service>.ledger.json.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UnitStatus is one plan unit's lifecycle state.
type UnitStatus string

const (
	StatusPlanned   UnitStatus = "planned"
	StatusGenerated UnitStatus = "generated"
	StatusValidated UnitStatus = "validated"
	StatusAppended  UnitStatus = "appended"
	StatusFailed    UnitStatus = "failed"
	StatusBlocked   UnitStatus = "blocked"
	StatusSkipped   UnitStatus = "skipped"
)

// Entry is one unit's durable state.
type Entry struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Status   UnitStatus `json:"status"`
	Attempts int        `json:"attempts"`
	Error    string     `json:"error,omitempty"`
	Targets  []string   `json:"targets,omitempty"`
}

// MapEntry links one legacy section to its generated artifact (§4.6) — the
// reverse index of the provenance headers.
type MapEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Ledger is the durable conversion state for one service.
type Ledger struct {
	Service string            `json:"service"`
	Units   map[string]*Entry `json:"units"`
	Map     []MapEntry        `json:"map"`

	path string
}

// Load reads <dir>/<service>.ledger.json, or returns a fresh ledger when
// absent — resume-safe by construction.
func Load(dir, service string) (*Ledger, error) {
	l := &Ledger{Service: service, Units: map[string]*Entry{}, path: filepath.Join(dir, service+".ledger.json")}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("ledger: read %s: %w", l.path, err)
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("ledger: parse %s: %w", l.path, err)
	}
	if l.Units == nil {
		l.Units = map[string]*Entry{}
	}
	l.path = filepath.Join(dir, service+".ledger.json")
	return l, nil
}

// Get returns the unit's entry, creating a planned one on first touch.
func (l *Ledger) Get(id, kind, name string) *Entry {
	if e, ok := l.Units[id]; ok {
		return e
	}
	e := &Entry{ID: id, Kind: kind, Name: name, Status: StatusPlanned}
	l.Units[id] = e
	return e
}

// Set transitions a unit and records the outcome.
func (l *Ledger) Set(id string, status UnitStatus, errMsg string, targets ...string) {
	e := l.Units[id]
	if e == nil {
		e = &Entry{ID: id}
		l.Units[id] = e
	}
	e.Status = status
	e.Error = errMsg
	if len(targets) > 0 {
		e.Targets = targets
	}
}

// AddMap records one source→target link (§4.6). Completeness: the convert
// pipeline adds one entry per generated artifact.
func (l *Ledger) AddMap(source, target string) {
	l.Map = append(l.Map, MapEntry{Source: source, Target: target})
}

// Save persists the ledger (best-effort atomic: temp file + rename).
func (l *Ledger) Save() error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger: marshal: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("ledger: mkdir: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("ledger: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("ledger: rename: %w", err)
	}
	return nil
}

// Counts summarizes the ledger for run summaries.
func (l *Ledger) Counts() (appended, failed, blocked, skipped int) {
	for _, e := range l.Units {
		switch e.Status {
		case StatusAppended, StatusValidated:
			appended++
		case StatusFailed:
			failed++
		case StatusBlocked:
			blocked++
		case StatusSkipped:
			skipped++
		}
	}
	return
}
