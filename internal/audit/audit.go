// Package audit implements the append-only JSONL log from §11: one line per
// tool call and per write, which is the durable record. The SQLite `audit`
// table is a queryable mirror of this, never the other way round.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one audited event.
type Record struct {
	At     time.Time      `json:"at"`
	Event  string         `json:"event"` // e.g. "tool.call", "config.write"
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Result string         `json:"result,omitempty"` // ok | error
	Error  string         `json:"error,omitempty"`
	Code   string         `json:"code,omitempty"` // stable error code from §7
	PID    int            `json:"pid"`
}

// Log appends records to a daily file under <stateDir>/audit.
type Log struct {
	dir string
	mu  sync.Mutex
}

// Open prepares the audit directory.
func Open(stateDir string) (*Log, error) {
	dir := filepath.Join(stateDir, "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	return &Log{dir: dir}, nil
}

// Dir returns the audit directory, for status output.
func (l *Log) Dir() string { return l.dir }

// Write appends one record. O_APPEND with a single write keeps lines from
// interleaving even when a timer run and a session share the file.
func (l *Log) Write(rec Record) error {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	if rec.PID == 0 {
		rec.PID = os.Getpid()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	path := filepath.Join(l.dir, rec.At.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}

// Tool records a tool invocation outcome in one call.
func (l *Log) Tool(name string, args map[string]any, err error, code string) error {
	rec := Record{Event: "tool.call", Tool: name, Args: args, Result: "ok"}
	if err != nil {
		rec.Result = "error"
		rec.Error = err.Error()
	}
	rec.Code = code
	return l.Write(rec)
}
