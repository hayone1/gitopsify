package server

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AuditEvent is one entry of the audit log.
type AuditEvent struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`
	Via     string    `json:"via,omitempty"`
	Action  string    `json:"action"`
	Outcome string    `json:"outcome"` // ok, denied, error
	Cluster string    `json:"cluster,omitempty"`
	Target  string    `json:"target,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	IP      string    `json:"ip,omitempty"`
	Request string    `json:"request,omitempty"`
}

// auditLog appends events to one JSON-lines file per day.
type auditLog struct {
	dir       string
	stdout    bool
	retention time.Duration
	log       *slog.Logger

	mu   sync.Mutex
	day  string
	file *os.File
}

func newAuditLog(dir string, stdout bool, retention time.Duration, log *slog.Logger) (*auditLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &auditLog{dir: dir, stdout: stdout, retention: retention, log: log}, nil
}

func (a *auditLog) record(e AuditEvent) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if a.stdout {
		a.log.Info("audit", "actor", e.Actor, "action", e.Action, "outcome", e.Outcome, "cluster", e.Cluster, "target", e.Target, "detail", e.Detail)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	day := e.Time.Format("2006-01-02")
	if a.file == nil || a.day != day {
		if a.file != nil {
			_ = a.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(a.dir, "audit-"+day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			a.log.Error("audit log", "error", err)
			a.file = nil
			return
		}
		a.file, a.day = f, day
		go a.prune()
	}
	if _, err := a.file.Write(append(b, '\n')); err != nil {
		a.log.Error("audit log", "error", err)
	}
}

// prune removes files older than the retention.
func (a *auditLog) prune() {
	files, _ := filepath.Glob(filepath.Join(a.dir, "audit-*.jsonl"))
	cutoff := time.Now().Add(-a.retention).Format("2006-01-02")
	for _, f := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "audit-"), ".jsonl")
		if day < cutoff {
			_ = os.Remove(f)
		}
	}
}

// auditQuery filters the log.
type auditQuery struct {
	Actor, Action, Cluster, Outcome string
	Before                          time.Time
	Limit                           int
}

// query returns the newest events matching q, newest first.
func (a *auditLog) query(q auditQuery) ([]AuditEvent, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	files, err := filepath.Glob(filepath.Join(a.dir, "audit-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	var out []AuditEvent
	for _, f := range files {
		evs, err := readAudit(f)
		if err != nil {
			return nil, err
		}
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			if !q.Before.IsZero() && !e.Time.Before(q.Before) {
				continue
			}
			if (q.Actor != "" && !strings.Contains(strings.ToLower(e.Actor), strings.ToLower(q.Actor))) ||
				(q.Action != "" && !strings.HasPrefix(e.Action, q.Action)) ||
				(q.Cluster != "" && e.Cluster != q.Cluster) ||
				(q.Outcome != "" && e.Outcome != q.Outcome) {
				continue
			}
			out = append(out, e)
			if len(out) == q.Limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func readAudit(path string) ([]AuditEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func (a *auditLog) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file != nil {
		_ = a.file.Close()
		a.file = nil
	}
}
