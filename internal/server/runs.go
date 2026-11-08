package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Run types and statuses.
const (
	RunScan    = "scan"
	RunMigrate = "migrate"
	RunExport  = "export"

	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Run is one piece of work: a scan, a migration or an export.
type Run struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Cluster string `json:"cluster,omitempty"`
	// Tool is the tool an export read (exports have no cluster).
	Tool string `json:"tool,omitempty"`
	// Repo is where an export's output is pushed (migrations name theirs in
	// their spec).
	Repo       *RunRepo        `json:"repo,omitempty"`
	Status     string          `json:"status"`
	CreatedBy  string          `json:"createdBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Error      string          `json:"error,omitempty"`
	Spec       json.RawMessage `json:"spec,omitempty"`
	Summary    *RunSummary     `json:"summary,omitempty"`
	Publishes  []Publish       `json:"publishes,omitempty"`
	// From is the scan a migration was made from.
	From string `json:"from,omitempty"`
}

// RunRepo is a Git repository and branch.
type RunRepo struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
}

// RunSummary is what a run found or wrote.
type RunSummary struct {
	Apps          int `json:"apps"`
	Objects       int `json:"objects"`
	Excluded      int `json:"excluded"`
	Files         int `json:"files,omitempty"`
	Warnings      int `json:"warnings"`
	SecretActions int `json:"secretActions"`
}

// Publish is one push of a run's output.
type Publish struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Dir    string    `json:"dir,omitempty"`
	Remote string    `json:"remote"`
	Branch string    `json:"branch"`
	Commit string    `json:"commit,omitempty"`
	Pushed bool      `json:"pushed"`
	Error  string    `json:"error,omitempty"`
}

// LogLine is one line of a run's log.
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// runEvent is what subscribers of a run receive: a log line (with its
// sequence number) or the run.
type runEvent struct {
	Log *LogLine
	Seq int
	Run *Run
}

func (r *Run) finished() bool {
	return r.Status == StatusSucceeded || r.Status == StatusFailed || r.Status == StatusCanceled
}

const (
	outputBlob    = "output.tar.gz"
	heartbeatTick = 15 * time.Second
	// staleAfter is how long a run may go without a heartbeat before
	// another replica fails it (its pod died).
	staleAfter = 2 * time.Minute
)

// runs works on runs and keeps them in the store. Runs this process works
// on live in memory too (their cancel function, live subscribers); others
// (from other replicas, or finished) come from the store. Outputs are
// written to dir/<id>/output; with a shared store they are archived there
// too and unpacked on demand by other replicas.
type runs struct {
	store     store
	dir       string
	owner     string
	timeout   time.Duration
	retention time.Duration
	sem       chan struct{}
	log       *slog.Logger
	metrics   *metrics
	// finished is told about every run that finishes (notifications).
	finished func(Run)

	mu     sync.Mutex
	local  map[string]*runState
	closed bool
	wg     sync.WaitGroup
	stop   chan struct{}
	unpack sync.Mutex
}

type runState struct {
	mu     sync.Mutex
	run    Run
	lines  []LogLine
	subs   map[chan runEvent]struct{}
	cancel context.CancelFunc
}

func newRuns(st store, dir string, concurrency int, timeout, retention time.Duration, log *slog.Logger, m *metrics) (*runs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	rs := &runs{store: st, dir: dir, owner: firstNonEmpty(host, "gitopsify") + "-" + randomString(4), timeout: timeout,
		retention: retention, sem: make(chan struct{}, concurrency), log: log, metrics: m, local: map[string]*runState{},
		stop: make(chan struct{})}
	rs.reap(time.Now().Add(-staleAfter))
	go rs.loop()
	return rs, nil
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// reap fails unfinished runs nobody works on any more: with one replica,
// every one the process doesn't know; with a shared store, those without a
// recent heartbeat.
func (rs *runs) reap(before time.Time) {
	ctx, cancel := bg()
	defer cancel()
	stale, err := rs.store.staleRuns(ctx, before)
	if err != nil {
		rs.log.Warn("looking for abandoned runs", "error", err)
		return
	}
	for _, r := range stale {
		rs.mu.Lock()
		_, mine := rs.local[r.ID]
		rs.mu.Unlock()
		if mine {
			continue
		}
		now := time.Now().UTC()
		r.Status, r.Error, r.FinishedAt = StatusFailed, "the server working on it stopped", &now
		if err := rs.store.saveRun(ctx, &r, ""); err != nil {
			rs.log.Warn("failing an abandoned run", "run", r.ID, "error", err)
		}
	}
}

// loop applies cancel requests from other replicas, keeps the heartbeat of
// local runs and fails abandoned ones.
func (rs *runs) loop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	var beat, reaped time.Time
	for {
		select {
		case <-rs.stop:
			return
		case <-t.C:
		}
		rs.mu.Lock()
		ids := make([]string, 0, len(rs.local))
		for id := range rs.local {
			ids = append(ids, id)
		}
		rs.mu.Unlock()
		ctx, cancel := bg()
		if len(ids) > 0 {
			if cancels, err := rs.store.cancelRequested(ctx, ids); err == nil {
				for _, id := range cancels {
					rs.cancelLocal(id)
				}
			}
			if time.Since(beat) > heartbeatTick {
				if err := rs.store.heartbeat(ctx, ids); err != nil {
					rs.log.Warn("run heartbeat", "error", err)
				}
				beat = time.Now()
			}
		}
		cancel()
		if rs.store.shared() && time.Since(reaped) > time.Minute {
			rs.reap(time.Now().Add(-staleAfter))
			reaped = time.Now()
		}
	}
}

func (rs *runs) runDir(id string) string { return filepath.Join(rs.dir, id) }

// outputDir holds a run's generated files on this replica.
func (rs *runs) outputDir(id string) string { return filepath.Join(rs.dir, id, "output") }

func newRunID() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + strings.ToLower(randomString(4)[:5])
}

// runContext is what a run's work uses to report.
type runContext struct {
	rs *runs
	st *runState
}

func (rc *runContext) ID() string { return rc.st.run.ID }

func (rc *runContext) logf(level, format string, args ...any) {
	line := LogLine{Time: time.Now().UTC(), Level: level, Message: fmt.Sprintf(format, args...)}
	st := rc.st
	st.mu.Lock()
	seq := len(st.lines)
	st.lines = append(st.lines, line)
	for ch := range st.subs {
		select {
		case ch <- runEvent{Log: &line, Seq: seq}:
		default: // a slow subscriber misses lines rather than blocking the run
		}
	}
	id := st.run.ID
	st.mu.Unlock()
	ctx, cancel := bg()
	defer cancel()
	if err := rc.rs.store.appendLog(ctx, id, seq, line); err != nil {
		rc.rs.log.Warn("run log", "run", id, "error", err)
	}
}

// Info logs progress, the way pipeline.Options.Log is called.
func (rc *runContext) Info(format string, args ...any) { rc.logf("info", format, args...) }
func (rc *runContext) Warn(format string, args ...any) { rc.logf("warn", format, args...) }

func (rc *runContext) update(fn func(*Run)) { rc.rs.update(rc.st, fn) }

func (rs *runs) update(st *runState, fn func(*Run)) {
	st.mu.Lock()
	fn(&st.run)
	r := st.run
	for ch := range st.subs {
		rr := r
		select {
		case ch <- runEvent{Run: &rr}:
		default:
		}
	}
	st.mu.Unlock()
	ctx, cancel := bg()
	defer cancel()
	if err := rs.store.saveRun(ctx, &r, rs.owner); err != nil {
		rs.log.Error("saving run", "run", r.ID, "error", err)
	}
}

// start records a run and does work in the background. work's error fails
// the run; a canceled context cancels it.
func (rs *runs) start(r Run, work func(context.Context, *runContext) error) (*Run, error) {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil, errors.New("the server is shutting down")
	}
	rs.mu.Unlock()
	r.ID = newRunID()
	r.Status = StatusQueued
	r.CreatedAt = time.Now().UTC()
	if err := os.MkdirAll(rs.runDir(r.ID), 0o700); err != nil {
		return nil, err
	}
	sctx, scancel := bg()
	err := rs.store.saveRun(sctx, &r, rs.owner)
	scancel()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rs.timeout)
	st := &runState{run: r, subs: map[chan runEvent]struct{}{}, cancel: cancel}
	rs.mu.Lock()
	rs.local[r.ID] = st
	rs.wg.Add(1)
	rs.mu.Unlock()
	rs.metrics.runStarted(r.Type)

	go func() {
		defer rs.wg.Done()
		defer cancel()
		rc := &runContext{rs: rs, st: st}
		select {
		case rs.sem <- struct{}{}:
		case <-ctx.Done():
			rs.finish(rc, ctx.Err())
			return
		}
		defer func() { <-rs.sem }()
		now := time.Now().UTC()
		rc.update(func(r *Run) { r.Status, r.StartedAt = StatusRunning, &now })
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					rs.log.Error("run panicked", "run", r.ID, "panic", p)
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return work(ctx, rc)
		}()
		// The work's own result decides: a push that finished just as the
		// run was canceled still happened.
		rs.finish(rc, err)
	}()
	return &r, nil
}

func (rs *runs) finish(rc *runContext, err error) {
	now := time.Now().UTC()
	status := StatusSucceeded
	msg := ""
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		status, msg = StatusCanceled, "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, msg = StatusFailed, fmt.Sprintf("timed out after %s", rs.timeout)
	default:
		status, msg = StatusFailed, err.Error()
	}
	if msg != "" {
		rc.logf("error", "%s", msg)
	}
	id := rc.ID()
	if status == StatusSucceeded {
		if err := rs.saveOutput(id); err != nil {
			status, msg = StatusFailed, "storing the output: "+err.Error()
			rc.logf("error", "%s", msg)
		}
	}
	rc.update(func(r *Run) { r.Status, r.Error, r.FinishedAt = status, msg, &now })
	st := rc.st
	st.mu.Lock()
	for ch := range st.subs {
		close(ch)
	}
	st.subs = map[chan runEvent]struct{}{}
	r := st.run
	st.mu.Unlock()
	rs.mu.Lock()
	delete(rs.local, id)
	rs.mu.Unlock()
	rs.metrics.runFinished(r.Type, status)
	if rs.finished != nil {
		rs.finished(r)
	}
}

// saveOutput archives a run's output into a shared store, so every replica
// can serve and push it. The file store reads the directory itself.
func (rs *runs) saveOutput(id string) error {
	if !rs.store.shared() {
		return nil
	}
	dir := rs.outputDir(id)
	if _, err := os.Stat(dir); err != nil {
		return nil // scans write no files
	}
	data, err := packDir(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return rs.store.putBlob(ctx, id, outputBlob, data)
}

// ensureOutput returns the directory of a run's output on this replica,
// unpacking it from a shared store when another replica wrote it.
func (rs *runs) ensureOutput(ctx context.Context, id string) (string, error) {
	dir := rs.outputDir(id)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if !rs.store.shared() {
		return "", errNotFound
	}
	rs.unpack.Lock()
	defer rs.unpack.Unlock()
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	data, err := rs.store.getBlob(ctx, id, outputBlob)
	if err != nil {
		return "", err
	}
	tmp := dir + ".unpack"
	_ = os.RemoveAll(tmp)
	if err := unpackDir(data, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// get returns a run: this process's copy while it works on it, the store's
// otherwise.
func (rs *runs) get(id string) (*Run, bool) {
	rs.mu.Lock()
	st, ok := rs.local[id]
	rs.mu.Unlock()
	if ok {
		st.mu.Lock()
		defer st.mu.Unlock()
		r := st.run
		return &r, true
	}
	ctx, cancel := bg()
	defer cancel()
	r, err := rs.store.getRun(ctx, id)
	if err != nil {
		return nil, false
	}
	return r, true
}

// list returns runs newest first, keeping those keep accepts.
func (rs *runs) list(keep func(*Run) bool) []Run {
	ctx, cancel := bg()
	defer cancel()
	all, err := rs.store.listRuns(ctx)
	if err != nil {
		rs.log.Error("listing runs", "error", err)
		return nil
	}
	var out []Run
	for i := range all {
		r := all[i]
		if keep(&r) {
			r.Spec = nil // lists stay small; the run itself has it
			out = append(out, r)
		}
	}
	return out
}

func (rs *runs) cancelLocal(id string) bool {
	rs.mu.Lock()
	st, ok := rs.local[id]
	rs.mu.Unlock()
	if !ok {
		return false
	}
	st.mu.Lock()
	cancel, done := st.cancel, st.run.finished()
	st.mu.Unlock()
	if done || cancel == nil {
		return false
	}
	cancel()
	return true
}

// cancel stops a run that hasn't finished, here or on another replica.
func (rs *runs) cancel(id string) bool {
	if rs.cancelLocal(id) {
		return true
	}
	r, ok := rs.get(id)
	if !ok || r.finished() {
		return false
	}
	ctx, cancel := bg()
	defer cancel()
	return rs.store.requestCancel(ctx, id) == nil
}

// remove deletes a finished run and its files.
func (rs *runs) remove(id string) error {
	r, ok := rs.get(id)
	if !ok {
		return os.ErrNotExist
	}
	if !r.finished() {
		return errors.New("the run hasn't finished; cancel it first")
	}
	ctx, cancel := bg()
	defer cancel()
	if err := rs.store.deleteRun(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(rs.runDir(id))
}

// subscribe returns the run and its log so far, taken together, and a
// channel of what follows; the channel closes when the run finishes or ctx
// ends. A nil channel means the run has finished.
func (rs *runs) subscribe(ctx context.Context, id string) (run *Run, lines []LogLine, ch chan runEvent, unsubscribe func(), ok bool) {
	rs.mu.Lock()
	st, local := rs.local[id]
	rs.mu.Unlock()
	if local {
		st.mu.Lock()
		if !st.run.finished() {
			r := st.run
			lines = append([]LogLine(nil), st.lines...)
			ch = make(chan runEvent, 256)
			st.subs[ch] = struct{}{}
			st.mu.Unlock()
			return &r, lines, ch, func() {
				st.mu.Lock()
				if _, ok := st.subs[ch]; ok {
					delete(st.subs, ch)
					close(ch)
				}
				st.mu.Unlock()
			}, true
		}
		st.mu.Unlock()
	}
	r, found := rs.get(id)
	if !found {
		return nil, nil, nil, nil, false
	}
	sctx, cancel := bg()
	lines, err := rs.store.readLog(sctx, id, 0)
	cancel()
	if err != nil {
		rs.log.Warn("reading a run log", "run", id, "error", err)
	}
	if r.finished() {
		return r, lines, nil, func() {}, true
	}
	// Another replica works on it: follow the store.
	ch = make(chan runEvent, 256)
	pctx, stop := context.WithCancel(ctx)
	go rs.poll(pctx, id, len(lines), r.Status, ch)
	return r, lines, ch, stop, true
}

func (rs *runs) poll(ctx context.Context, id string, seq int, status string, ch chan runEvent) {
	defer close(ch)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sctx, cancel := bg()
		lines, err := rs.store.readLog(sctx, id, seq)
		r, gerr := rs.store.getRun(sctx, id)
		cancel()
		if err == nil {
			for i := range lines {
				select {
				case ch <- runEvent{Log: &lines[i], Seq: seq + i}:
				case <-ctx.Done():
					return
				}
			}
			seq += len(lines)
		}
		if gerr != nil {
			return
		}
		if r.Status != status {
			status = r.Status
			select {
			case ch <- runEvent{Run: r}:
			case <-ctx.Done():
				return
			}
		}
		if r.finished() {
			return
		}
	}
}

// readLog returns a run's whole log.
func (rs *runs) readLog(id string) []LogLine {
	ctx, cancel := bg()
	defer cancel()
	lines, _ := rs.store.readLog(ctx, id, 0)
	return lines
}

func (rs *runs) putBlob(id, name string, data []byte) error {
	ctx, cancel := bg()
	defer cancel()
	return rs.store.putBlob(ctx, id, name, data)
}

func (rs *runs) getBlob(id, name string) ([]byte, error) {
	ctx, cancel := bg()
	defer cancel()
	return rs.store.getBlob(ctx, id, name)
}

// prune removes finished runs older than the retention.
func (rs *runs) prune() {
	cutoff := time.Now().Add(-rs.retention)
	for _, r := range rs.list(func(r *Run) bool { return r.finished() && r.FinishedAt != nil && r.FinishedAt.Before(cutoff) }) {
		if err := rs.remove(r.ID); err != nil {
			rs.log.Warn("pruning run", "run", r.ID, "error", err)
		}
	}
}

// shutdown cancels running work and waits for it.
func (rs *runs) shutdown(ctx context.Context) {
	rs.mu.Lock()
	if !rs.closed {
		close(rs.stop)
	}
	rs.closed = true
	for _, st := range rs.local {
		st.mu.Lock()
		if st.cancel != nil && !st.run.finished() {
			st.cancel()
		}
		st.mu.Unlock()
	}
	rs.mu.Unlock()
	done := make(chan struct{})
	go func() { rs.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
