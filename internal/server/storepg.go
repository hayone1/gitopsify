package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	`CREATE INDEX IF NOT EXISTS gitopsify_runs_page ON gitopsify_runs (created_at DESC, id DESC)`,
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
	`CREATE INDEX IF NOT EXISTS gitopsify_runs_unfinished ON gitopsify_runs (heartbeat) WHERE finished_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS gitopsify_runs_finished ON gitopsify_runs (finished_at) WHERE finished_at IS NOT NULL`,
	// A run's lock is a lease (no connection is held while pushing).
	`ALTER TABLE gitopsify_runs ADD COLUMN IF NOT EXISTS lock_owner text NOT NULL DEFAULT ''`,
	`ALTER TABLE gitopsify_runs ADD COLUMN IF NOT EXISTS lock_until timestamptz`,
}

// lockLease is how long a run's lock lasts unless renewed: a replica that
// dies while pushing blocks the run for that long (a variable for tests).
var lockLease = 2 * time.Minute

// errLockLost ends a push whose lock lease couldn't be renewed in time.
var errLockLost = errors.New("the run's lock was lost (the database was unreachable for too long): another replica may push it now; push again")

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

func (p *pgStore) createRun(ctx context.Context, r *Run, owner string) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO gitopsify_runs (id, data, status, created_at, finished_at, owner, heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6, now())`,
		r.ID, b, r.Status, r.CreatedAt, r.FinishedAt, owner)
	return err
}

func (p *pgStore) updateRun(ctx context.Context, r *Run, owner string) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE gitopsify_runs SET data = $2, status = $3, finished_at = $4, heartbeat = now()
		WHERE id = $1 AND owner = $5 AND finished_at IS NULL`,
		r.ID, b, r.Status, r.FinishedAt, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return errFenced
	}
	return err
}

func (p *pgStore) addPublishes(ctx context.Context, id string, ps []Publish, bump bool) (*Run, error) {
	var out *Run
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var b []byte
		err := tx.QueryRow(ctx, `SELECT data FROM gitopsify_runs WHERE id = $1 FOR UPDATE`, id).Scan(&b)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		var r Run
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if !addNewPublishes(&r, ps) {
			out = &r
			return nil
		}
		if bump {
			r.OutputVersion++
		}
		if b, err = json.Marshal(&r); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE gitopsify_runs SET data = $2 WHERE id = $1`, id, b); err != nil {
			return err
		}
		out = &r
		return nil
	})
	return out, err
}

// lockRun takes a lease on the run, renewed until unlock: no connection is
// held meanwhile (a pool of n connections would stall with n pushes). The
// context it returns ends if the lease is lost.
func (p *pgStore) lockRun(ctx context.Context, id string) (context.Context, func(), bool, error) {
	token := randomString(12)
	asked := time.Now()
	var exists, locked int
	err := p.pool.QueryRow(ctx, `
		WITH l AS (
			UPDATE gitopsify_runs SET lock_owner = $2, lock_until = now() + make_interval(secs => $3)
			WHERE id = $1 AND (lock_until IS NULL OR lock_until < now()) RETURNING id)
		SELECT (SELECT count(*) FROM gitopsify_runs WHERE id = $1), (SELECT count(*) FROM l)`,
		id, token, lockLease.Seconds()).Scan(&exists, &locked)
	switch {
	case err != nil:
		return nil, nil, false, err
	case exists == 0:
		return nil, nil, false, errNotFound
	case locked == 0:
		return nil, nil, false, nil
	}
	lctx, cancelLock := context.WithCancelCause(ctx)
	lost := func() { cancelLock(errLockLost) }
	lease := lockLease
	// The lock counts as lost half a lease after the last renewal that was
	// sent and acknowledged: well before it can expire in the database,
	// however long the database takes to answer.
	deadline := time.AfterFunc(lease/2-time.Since(asked), lost)
	isLost := func() bool { return errors.Is(context.Cause(lctx), errLockLost) }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer deadline.Stop()
		t := time.NewTicker(lease / 8)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// After the caller's context ends (not the lease), renewals go
				// on until unlock: the UPDATE still needs our token and an
				// unexpired lease, so it never takes a lock back.
				if isLost() {
					return
				}
				sent := time.Now()
				rctx, cancel := context.WithTimeout(context.Background(), lease/4)
				tag, err := p.pool.Exec(rctx, `UPDATE gitopsify_runs SET lock_until = now() + make_interval(secs => $3)
					WHERE id = $1 AND lock_owner = $2 AND lock_until > now()`, id, token, lease.Seconds())
				cancel()
				switch {
				case err == nil && tag.RowsAffected() == 0:
					lost() // expired, or the run is gone
					return
				case err == nil && !isLost():
					// Valid until sent+lease in the database's time: lost
					// half a lease from sending.
					deadline.Reset(lease/2 - time.Since(sent))
				}
			}
		}
	}()
	var once sync.Once
	return lctx, func() {
		once.Do(func() {
			close(stop)
			<-done
			cancelLock(context.Canceled)
			uctx, cancel := bg()
			defer cancel()
			_, _ = p.pool.Exec(uctx, `UPDATE gitopsify_runs SET lock_until = NULL, lock_owner = '' WHERE id = $1 AND lock_owner = $2`, id, token)
		})
	}, true, nil
}

func (p *pgStore) pruneRuns(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := p.pool.Query(ctx, `DELETE FROM gitopsify_runs
		WHERE finished_at < $1 AND (lock_until IS NULL OR lock_until < now()) RETURNING id`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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

func (p *pgStore) listRuns(ctx context.Context, q runQuery) ([]Run, error) {
	sql := `SELECT data::jsonb - 'spec' FROM gitopsify_runs WHERE true`
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	// PostgreSQL keeps microseconds; runs are created with no more.
	q.Before = q.Before.Truncate(time.Microsecond)
	if !q.Before.IsZero() && q.BeforeID != "" {
		sql += ` AND (created_at, id) < (` + arg(q.Before) + `, ` + arg(q.BeforeID) + `)`
	} else if !q.Before.IsZero() {
		sql += ` AND created_at < ` + arg(q.Before)
	}
	if q.Type != "" {
		sql += ` AND data->>'type' = ` + arg(q.Type)
	}
	if q.Cluster != "" {
		sql += ` AND data->>'cluster' = ` + arg(q.Cluster)
	}
	if q.Status != "" {
		sql += ` AND status = ` + arg(q.Status)
	}
	if q.CreatedBy != "" {
		sql += ` AND data->>'createdBy' = ` + arg(q.CreatedBy)
	}
	if q.Active {
		sql += ` AND finished_at IS NULL`
	}
	sql += ` ORDER BY created_at DESC, id DESC`
	if q.Limit > 0 {
		sql += ` LIMIT ` + arg(q.Limit)
	}
	rows, err := p.pool.Query(ctx, sql, args...)
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

func (p *pgStore) heartbeat(ctx context.Context, owner string, ids []string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `UPDATE gitopsify_runs SET heartbeat = now()
		WHERE id = ANY($1) AND owner = $2 AND finished_at IS NULL RETURNING id`, ids, owner)
	if err != nil {
		return nil, err
	}
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var lost []string
	for _, id := range ids {
		if !contains(kept, id) {
			lost = append(lost, id)
		}
	}
	return lost, nil
}

// reapRuns compares heartbeats with the database's clock, and locks the
// rows it fails: two replicas never fail (and notify about) the same run.
func (p *pgStore) reapRuns(ctx context.Context, keep []string, staleAfter time.Duration, fail func(*Run)) ([]Run, error) {
	var out []Run
	if keep == nil {
		keep = []string{}
	}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM gitopsify_runs
			WHERE finished_at IS NULL AND NOT (id = ANY($1)) AND heartbeat < now() - make_interval(secs => $2)
			FOR UPDATE SKIP LOCKED`, keep, staleAfter.Seconds())
		if err != nil {
			return err
		}
		datas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, b := range datas {
			var r Run
			if json.Unmarshal(b, &r) != nil {
				continue
			}
			fail(&r)
			nb, err := json.Marshal(&r)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE gitopsify_runs SET data = $2, status = $3, finished_at = $4 WHERE id = $1`,
				r.ID, nb, r.Status, r.FinishedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
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
