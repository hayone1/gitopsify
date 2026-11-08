package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore keeps runs, logs, outputs and the audit log in PostgreSQL, shared
// by every replica.
type pgStore struct {
	pool *pgxpool.Pool
}

// pgSchema creates the tables; every statement is idempotent, and replicas
// starting together take an advisory lock first.
var pgSchema = []string{
	`CREATE TABLE IF NOT EXISTS gitopsify_runs (
		id text PRIMARY KEY,
		-- json, not jsonb: the spec keeps its text and order.
		data json NOT NULL,
		status text NOT NULL,
		created_at timestamptz NOT NULL,
		finished_at timestamptz,
		owner text NOT NULL DEFAULT '',
		heartbeat timestamptz NOT NULL DEFAULT now(),
		cancel_requested boolean NOT NULL DEFAULT false)`,
	`CREATE INDEX IF NOT EXISTS gitopsify_runs_created ON gitopsify_runs (created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS gitopsify_run_logs (
		run_id text NOT NULL REFERENCES gitopsify_runs (id) ON DELETE CASCADE,
		seq integer NOT NULL,
		line jsonb NOT NULL,
		PRIMARY KEY (run_id, seq))`,
	`CREATE TABLE IF NOT EXISTS gitopsify_run_blobs (
		run_id text NOT NULL REFERENCES gitopsify_runs (id) ON DELETE CASCADE,
		name text NOT NULL,
		data bytea NOT NULL,
		PRIMARY KEY (run_id, name))`,
	`CREATE TABLE IF NOT EXISTS gitopsify_audit (
		id bigserial PRIMARY KEY,
		time timestamptz NOT NULL,
		actor text NOT NULL DEFAULT '',
		action text NOT NULL DEFAULT '',
		outcome text NOT NULL DEFAULT '',
		cluster text NOT NULL DEFAULT '',
		event jsonb NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gitopsify_audit_time ON gitopsify_audit (time DESC)`,
}

const pgLock = 7041993 // advisory lock id for schema changes

func newPGStore(ctx context.Context, url string, maxConns int) (*pgStore, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("spec.storage.postgres.url: %w", redactPG(err))
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", redactPG(err))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", redactPG(err))
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, pgLock); err != nil {
		pool.Close()
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, pgLock) //nolint:errcheck
	for _, stmt := range pgSchema {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("creating the tables: %w", err)
		}
	}
	return &pgStore{pool: pool}, nil
}

// redactPG keeps passwords out of connection errors.
func redactPG(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "://"); i >= 0 {
		if at := strings.Index(msg[i:], "@"); at > 0 {
			msg = msg[:i+3] + "xxxxx" + msg[i+at:]
		}
	}
	if i := strings.Index(msg, "password="); i >= 0 {
		end := strings.IndexAny(msg[i:], " &")
		if end < 0 {
			end = len(msg) - i
		}
		msg = msg[:i] + "password=xxxxx" + msg[i+end:]
	}
	return errors.New(msg)
}

func (p *pgStore) shared() bool { return true }

func (p *pgStore) saveRun(ctx context.Context, r *Run, owner string) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO gitopsify_runs (id, data, status, created_at, finished_at, owner, heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (id) DO UPDATE SET data = $2, status = $3, finished_at = $5,
			owner = CASE WHEN $6 = '' THEN gitopsify_runs.owner ELSE $6 END, heartbeat = now()`,
		r.ID, b, r.Status, r.CreatedAt, r.FinishedAt, owner)
	return err
}

func (p *pgStore) getRun(ctx context.Context, id string) (*Run, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT data FROM gitopsify_runs WHERE id = $1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (p *pgStore) listRuns(ctx context.Context) ([]Run, error) {
	rows, err := p.pool.Query(ctx, `SELECT data::jsonb - 'spec' FROM gitopsify_runs ORDER BY created_at DESC LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var r Run
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (p *pgStore) deleteRun(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM gitopsify_runs WHERE id = $1`, id)
	return err
}

func (p *pgStore) appendLog(ctx context.Context, id string, seq int, l LogLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO gitopsify_run_logs (run_id, seq, line) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, seq, b)
	return err
}

func (p *pgStore) readLog(ctx context.Context, id string, from int) ([]LogLine, error) {
	rows, err := p.pool.Query(ctx, `SELECT line FROM gitopsify_run_logs WHERE run_id = $1 AND seq >= $2 ORDER BY seq`, id, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogLine
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var l LogLine
		if json.Unmarshal(b, &l) == nil {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

func (p *pgStore) putBlob(ctx context.Context, id, name string, data []byte) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO gitopsify_run_blobs (run_id, name, data) VALUES ($1, $2, $3)
		ON CONFLICT (run_id, name) DO UPDATE SET data = $3`, id, name, data)
	return err
}

func (p *pgStore) getBlob(ctx context.Context, id, name string) ([]byte, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT data FROM gitopsify_run_blobs WHERE run_id = $1 AND name = $2`, id, name).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return b, err
}

func (p *pgStore) requestCancel(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `UPDATE gitopsify_runs SET cancel_requested = true WHERE id = $1`, id)
	return err
}

func (p *pgStore) cancelRequested(ctx context.Context, ids []string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT id FROM gitopsify_runs WHERE id = ANY($1) AND cancel_requested`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *pgStore) heartbeat(ctx context.Context, ids []string) error {
	_, err := p.pool.Exec(ctx, `UPDATE gitopsify_runs SET heartbeat = now() WHERE id = ANY($1)`, ids)
	return err
}

func (p *pgStore) staleRuns(ctx context.Context, before time.Time) ([]Run, error) {
	rows, err := p.pool.Query(ctx, `SELECT data FROM gitopsify_runs WHERE finished_at IS NULL AND status IN ('queued', 'running') AND heartbeat < $1`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var r Run
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (p *pgStore) appendAudit(ctx context.Context, e AuditEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO gitopsify_audit (time, actor, action, outcome, cluster, event) VALUES ($1, $2, $3, $4, $5, $6)`,
		e.Time, e.Actor, e.Action, e.Outcome, e.Cluster, b)
	return err
}

func (p *pgStore) queryAudit(ctx context.Context, q auditQuery) ([]AuditEvent, error) {
	sql := `SELECT event FROM gitopsify_audit WHERE true`
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if !q.Before.IsZero() {
		sql += ` AND time < ` + arg(q.Before)
	}
	if q.Actor != "" {
		sql += ` AND actor ILIKE ` + arg("%"+escapeLike(q.Actor)+"%")
	}
	if q.Action != "" {
		sql += ` AND action LIKE ` + arg(escapeLike(q.Action)+"%")
	}
	if q.Cluster != "" {
		sql += ` AND cluster = ` + arg(q.Cluster)
	}
	if q.Outcome != "" {
		sql += ` AND outcome = ` + arg(q.Outcome)
	}
	sql += ` ORDER BY time DESC, id DESC LIMIT ` + arg(q.Limit)
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e AuditEvent
		if json.Unmarshal(b, &e) == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (p *pgStore) pruneAudit(ctx context.Context, before time.Time) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM gitopsify_audit WHERE time < $1`, before)
	return err
}

func (p *pgStore) close() error {
	p.pool.Close()
	return nil
}
