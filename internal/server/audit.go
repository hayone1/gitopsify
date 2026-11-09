package server

import (
	"context"
	"log/slog"
	"strings"
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

// auditLog records events in the store, and in the server's log with
// stdout (for log collectors and SIEMs).
type auditLog struct {
	store     store
	stdout    bool
	retention time.Duration
	log       *slog.Logger
	pruned    time.Time
}

func newAuditLog(st store, stdout bool, retention time.Duration, log *slog.Logger) *auditLog {
	return &auditLog{store: st, stdout: stdout, retention: retention, log: log}
}

func (a *auditLog) record(e AuditEvent) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if a.stdout {
		a.log.Info("audit", "actor", e.Actor, "via", e.Via, "action", e.Action, "outcome", e.Outcome, "cluster", e.Cluster,
			"target", e.Target, "detail", e.Detail, "ip", e.IP, "request", e.Request)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.store.appendAudit(ctx, e); err != nil {
		// Never lose an event silently: it is at least in the log.
		a.log.Error("audit log", "error", err, "actor", e.Actor, "action", e.Action, "outcome", e.Outcome, "detail", e.Detail)
	}
}

// prune removes events older than the retention, at most once an hour.
func (a *auditLog) prune() {
	if time.Since(a.pruned) < time.Hour {
		return
	}
	a.pruned = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := a.store.pruneAudit(ctx, time.Now().Add(-a.retention)); err != nil {
		a.log.Warn("pruning the audit log", "error", err)
	}
}

// auditQuery filters the log.
type auditQuery struct {
	Actor, Action, Cluster, Outcome string
	Before                          time.Time
	Limit                           int
}

func (q auditQuery) matches(e AuditEvent) bool {
	if !q.Before.IsZero() && !e.Time.Before(q.Before) {
		return false
	}
	return (q.Actor == "" || strings.Contains(strings.ToLower(e.Actor), strings.ToLower(q.Actor))) &&
		(q.Action == "" || strings.HasPrefix(e.Action, q.Action)) &&
		(q.Cluster == "" || e.Cluster == q.Cluster) &&
		(q.Outcome == "" || e.Outcome == q.Outcome)
}

// query returns the newest events matching q, newest first.
func (a *auditLog) query(q auditQuery) ([]AuditEvent, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.store.queryAudit(ctx, q)
}
