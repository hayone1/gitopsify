package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/hayone1/gitopsify/internal/telemetry"
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
	// OutputVersion counts changes of the stored output (pushes commit
	// to it); replicas holding an older copy unpack it again.
	OutputVersion int `json:"outputVersion,omitempty"`
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
	// cacheAge is how long a replica keeps an unused copy of another
	// run's output (shared stores; it's unpacked again when needed).
	cacheAge = time.Hour
)

// errPushing is returned while a run's output is being pushed.
var errPushing = errors.New("this run is being pushed; try again when the push is done")

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
	// dirs locks a run's directory on this replica (unpacking, removing).
	dirs dirLocks
	// unsaved are finished runs whose final state couldn't be stored yet:
	// the loop retries, or they would stay "running" in the store.
	unsaved map[string]Run
}

type runState struct {
	mu     sync.Mutex
	run    Run
	lines  []LogLine
	subs   map[chan runEvent]struct{}
	cancel context.CancelFunc
	// lost is set when the run is no longer this replica's: deleted, or
	// failed by another replica that thought it abandoned.
	lost bool
	// interrupted is set when the server stopped it (a restart or update).
	interrupted bool
}

// errUnfinished refuses to delete a run that is still working.
var errUnfinished = errors.New("the run hasn't finished; cancel it first")

// errShuttingDown refuses runs once the server is stopping.
var errShuttingDown = errors.New("the server is shutting down; start the run again in a moment")

// serverError is a failure of the server (its disk, its database), not of
// the request.
type serverError struct{ error }

func (e serverError) Unwrap() error { return e.error }

// newRuns starts working on runs; finished (may be nil) is told about
// every run that finishes, including those failed as abandoned.
func newRuns(st store, dir string, concurrency int, timeout, retention time.Duration, log *slog.Logger, m *metrics, finished func(Run)) (*runs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	rs := &runs{store: st, dir: dir, owner: firstNonEmpty(host, "gitopsify") + "-" + randomString(4), timeout: timeout,
		retention: retention, sem: make(chan struct{}, concurrency), log: log, metrics: m, local: map[string]*runState{},
		stop: make(chan struct{}), finished: finished}
	rs.reap()
	go rs.loop()
	if st.shared() {
		rs.wg.Add(1)
		go func() { defer rs.wg.Done(); rs.evictLoop() }()
	}
	return rs, nil
}

// dirLocks are mutexes by run ID, dropped when unused.
type dirLocks struct {
	mu sync.Mutex
	m  map[string]*dirLock
}

type dirLock struct {
	sync.Mutex
	refs int
}

func (d *dirLocks) lock(id string) (unlock func()) {
	d.mu.Lock()
	if d.m == nil {
		d.m = map[string]*dirLock{}
	}
	l := d.m[id]
	if l == nil {
		l = &dirLock{}
		d.m[id] = l
	}
	l.refs++
	d.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		d.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(d.m, id)
		}
		d.mu.Unlock()
	}
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// reap fails unfinished runs nobody works on any more: with one replica,
// those of the process before a restart; with a shared store, those of
// other replicas without a recent heartbeat.
func (rs *runs) reap() {
	ctx, cancel := bg()
	defer cancel()
	rs.mu.Lock()
	keep := make([]string, 0, len(rs.local))
	for id := range rs.local {
		keep = append(keep, id)
	}
	rs.mu.Unlock()
	reaped, err := rs.store.reapRuns(ctx, keep, staleAfter, func(r *Run) {
		now := time.Now().UTC()
		r.Status, r.Error, r.FinishedAt = StatusFailed, "the server working on it stopped", &now
	})
	if err != nil {
		rs.log.Warn("looking for abandoned runs", "error", err)
		return
	}
	for _, r := range reaped {
		rs.log.Warn("failed an abandoned run", "run", r.ID)
		if rs.finished != nil {
			rs.finished(r)
		}
	}
}

// lose stops a run that is no longer this replica's.
func (rs *runs) lose(st *runState) {
	st.mu.Lock()
	first := !st.lost
	st.lost = true
	cancel, id := st.cancel, st.run.ID
	st.mu.Unlock()
	if first {
		rs.log.Warn("a run was deleted or taken over while working; stopping it", "run", id)
		if cancel != nil {
			cancel()
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
				rs.beat(ctx, ids)
				beat = time.Now()
			}
		}
		rs.saveUnsaved(ctx)
		cancel()
		if rs.store.shared() && time.Since(reaped) > time.Minute {
			rs.reap()
			reaped = time.Now()
		}
	}
}

// beat records that this replica still works on the runs, and stops those
// it lost.
func (rs *runs) beat(ctx context.Context, ids []string) {
	lost, err := rs.store.heartbeat(ctx, rs.owner, ids)
	if err != nil {
		rs.log.Warn("run heartbeat", "error", err)
	}
	for _, id := range lost {
		rs.mu.Lock()
		st, ok := rs.local[id]
		rs.mu.Unlock()
		if !ok {
			continue
		}
		// A run whose final state is being saved reads as lost (it is
		// finished in the store): saveFinal tells the two apart.
		st.mu.Lock()
		saving := st.run.finished()
		st.mu.Unlock()
		if !saving {
			rs.lose(st)
		}
	}
}

// saveUnsaved retries storing the final state of runs whose last save
// failed.
func (rs *runs) saveUnsaved(ctx context.Context) {
	rs.mu.Lock()
	pending := make([]Run, 0, len(rs.unsaved))
	for _, r := range rs.unsaved {
		pending = append(pending, r)
	}
	rs.mu.Unlock()
	for _, r := range pending {
		err := rs.store.updateRun(ctx, &r, rs.owner)
		if err != nil && !errors.Is(err, errFenced) && !errors.Is(err, errNotFound) {
			continue // still failing: next time
		}
		// Saved, or someone else decided (deleted, or failed it).
		rs.mu.Lock()
		delete(rs.unsaved, r.ID)
		rs.mu.Unlock()
	}
}

func (rs *runs) evictLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rs.stop:
			return
		case <-t.C:
			rs.evict(cacheAge)
		}
	}
}

// evict removes copies of other runs' outputs unused for maxAge (shared
// stores: the store has them; deleted runs' copies go too), never while
// the run is pushed.
func (rs *runs) evict(maxAge time.Duration) {
	entries, err := os.ReadDir(rs.dir)
	if err != nil {
		return
	}
	idle := func(id string) bool {
		info, err := os.Stat(rs.versionFile(id))
		if err != nil {
			info, err = os.Stat(rs.runDir(id))
		}
		return err == nil && time.Since(info.ModTime()) >= maxAge
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || strings.Contains(id, ".") || !idle(id) {
			continue
		}
		rs.mu.Lock()
		_, live := rs.local[id]
		rs.mu.Unlock()
		if live {
			continue
		}
		ctx, cancel := bg()
		_, unlock, ok, err := rs.store.lockRun(ctx, id)
		cancel()
		switch {
		case errors.Is(err, errNotFound): // deleted: its copy goes
		case err != nil || !ok:
			continue // being pushed, or the store is unreachable
		}
		release := rs.dirs.lock(id)
		if idle(id) {
			_ = os.RemoveAll(rs.runDir(id))
		}
		release()
		if unlock != nil {
			unlock()
		}
	}
}

func (rs *runs) runDir(id string) string { return filepath.Join(rs.dir, id) }

// outputDir holds a run's generated files on this replica.
func (rs *runs) outputDir(id string) string { return filepath.Join(rs.dir, id, "output") }

// versionFile holds the version of this replica's copy of the output, and
// its time when it was last used.
func (rs *runs) versionFile(id string) string { return filepath.Join(rs.dir, id, "output.version") }

func (rs *runs) markOutput(id string, version int) {
	_ = os.WriteFile(rs.versionFile(id), []byte(strconv.Itoa(version)), 0o600)
}

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
	line := LogLine{Time: time.Now().UTC(), Level: level, Message: logText(fmt.Sprintf(format, args...))}
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

// maxLogLine caps one line of a run's log (a provider's stderr can be MBs).
const maxLogLine = 64 << 10

// logText makes a message storable: PostgreSQL's jsonb and text refuse NUL
// characters, and one huge line would hide the rest of the log.
func logText(s string) string {
	s = noNUL(s)
	if len(s) > maxLogLine {
		s = strings.ToValidUTF8(s[:maxLogLine], "") + " … (cut)"
	}
	return s
}

// noNUL replaces NUL characters, which PostgreSQL's jsonb and text refuse.
func noNUL(s string) string { return strings.ReplaceAll(s, "\x00", "\uFFFD") }

// Info logs progress, the way pipeline.Options.Log is called.
func (rc *runContext) Info(format string, args ...any) { rc.logf("info", format, args...) }
func (rc *runContext) Warn(format string, args ...any) { rc.logf("warn", format, args...) }

func (rc *runContext) update(fn func(*Run)) { rc.rs.update(rc.st, fn) }

func (rs *runs) update(st *runState, fn func(*Run)) { _ = rs.save(st, fn) }

// saveFinal stores a run's last state, retried (or the run would stay
// "running" until another replica fails it as abandoned). A retry refused
// because the run has finished means an earlier attempt was stored after
// all, when its answer was lost.
func (rs *runs) saveFinal(st *runState, fn func(*Run)) {
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
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := rs.store.updateRun(ctx, &r, rs.owner)
		if errors.Is(err, errFenced) && attempt > 1 {
			// Ours (the same finish time, to the nanosecond), or another
			// replica's that failed it meanwhile? Unknown: try again.
			got, gerr := rs.store.getRun(ctx, r.ID)
			switch {
			case gerr == nil && got.FinishedAt != nil && r.FinishedAt != nil && got.FinishedAt.Equal(*r.FinishedAt):
				err = nil
			case gerr != nil && !errors.Is(gerr, errNotFound):
				err = gerr
			}
		}
		cancel()
		switch {
		case err == nil:
			return
		case errors.Is(err, errFenced):
			rs.lose(st)
			return
		case attempt == 4:
			rs.log.Error("saving a finished run; retrying in the background", "run", r.ID, "error", err)
			rs.mu.Lock()
			if rs.unsaved == nil {
				rs.unsaved = map[string]Run{}
			}
			rs.unsaved[r.ID] = r
			rs.mu.Unlock()
			return
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
}

// save changes a run and stores it; a run this replica lost is stopped.
func (rs *runs) save(st *runState, fn func(*Run)) error {
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
	err := rs.store.updateRun(ctx, &r, rs.owner)
	if errors.Is(err, errFenced) {
		rs.lose(st)
	} else if err != nil {
		rs.log.Error("saving run", "run", r.ID, "error", err)
	}
	return err
}

// start records a run and does work in the background. work's error fails
// the run; a canceled context cancels it. parent is the request that asked
// for it: the run's span joins its trace (runs outlive requests, so only the
// trace is shared, not the cancellation).
func (rs *runs) start(parent context.Context, r Run, work func(context.Context, *runContext) error) (*Run, error) {
	// The run counts from here, under the lock shutdown closes with: no
	// run starts after shutdown began, and shutdown waits for every run
	// that did (wg.Add never races its Wait).
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil, errShuttingDown
	}
	rs.wg.Add(1)
	rs.mu.Unlock()
	launched := false
	defer func() {
		if !launched {
			rs.wg.Done()
		}
	}()
	r.ID = newRunID()
	r.Status = StatusQueued
	// PostgreSQL keeps microseconds: paging by createdAt needs the same.
	r.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := os.MkdirAll(rs.runDir(r.ID), 0o700); err != nil {
		return nil, serverError{err}
	}
	sctx, scancel := bg()
	err := rs.store.createRun(sctx, &r, rs.owner)
	scancel()
	if err != nil {
		// The insert may have happened with its answer lost: don't leave a
		// queued run nobody works on (it would be reaped and notified).
		dctx, dcancel := bg()
		_ = rs.store.deleteRun(dctx, r.ID)
		dcancel()
		_ = os.RemoveAll(rs.runDir(r.ID))
		return nil, serverError{fmt.Errorf("recording the run: %w", err)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), rs.timeout)
	attrs := []attribute.KeyValue{attribute.String("gitopsify.run.id", r.ID), attribute.String("gitopsify.run.type", r.Type)}
	if r.Cluster != "" {
		attrs = append(attrs, attribute.String("gitopsify.cluster", r.Cluster))
	}
	if r.Tool != "" {
		attrs = append(attrs, attribute.String("gitopsify.tool", r.Tool))
	}
	ctx, span := telemetry.Tracer().Start(trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent)), "run "+r.Type,
		trace.WithAttributes(attrs...))
	st := &runState{run: r, subs: map[chan runEvent]struct{}{}, cancel: cancel}
	rs.mu.Lock()
	if rs.closed {
		// Shutdown began while the run was being recorded: it found no
		// run to stop, so this one ends at once, interrupted.
		st.interrupted = true
		cancel()
	}
	rs.local[r.ID] = st
	rs.mu.Unlock()
	rs.metrics.runStarted(r.Type)

	launched = true
	go func() {
		defer rs.wg.Done()
		defer cancel()
		rc := &runContext{rs: rs, st: st}
		end := func(status, msg string) {
			span.SetAttributes(attribute.String("gitopsify.run.status", status))
			if status != StatusSucceeded {
				span.SetStatus(codes.Error, msg)
			}
			span.End()
		}
		if ctx.Err() != nil { // stopped before it was queued
			end(rs.finish(rc, ctx.Err()))
			return
		}
		select {
		case rs.sem <- struct{}{}:
		case <-ctx.Done():
			end(rs.finish(rc, ctx.Err()))
			return
		}
		defer func() { <-rs.sem }()
		span.AddEvent("started")
		now := time.Now().UTC()
		rc.update(func(r *Run) { r.Status, r.StartedAt = StatusRunning, &now })
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					rs.log.ErrorContext(ctx, "run panicked", "run", r.ID, "panic", p)
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return work(ctx, rc)
		}()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// Some step's own deadline, not the run's timeout.
			err = errors.New(err.Error())
		}
		// The work's own result decides: a push that finished just as the
		// run was canceled still happened.
		end(rs.finish(rc, err))
	}()
	return &r, nil
}

// finish records how a run ended and returns its status and message.
func (rs *runs) finish(rc *runContext, err error) (string, string) {
	now := time.Now().UTC()
	status := StatusSucceeded
	msg := ""
	rc.st.mu.Lock()
	interrupted := rc.st.interrupted
	rc.st.mu.Unlock()
	switch {
	case err == nil:
	case interrupted && errors.Is(err, context.Canceled):
		status, msg = StatusFailed, "interrupted: the server stopped (a restart or an update); start it again"
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
	st := rc.st
	st.mu.Lock()
	lost := st.lost
	st.mu.Unlock()
	if status == StatusSucceeded && !lost {
		if err := rs.saveOutput(id); err != nil {
			status, msg = StatusFailed, "storing the output: "+err.Error()
			rc.logf("error", "%s", msg)
		}
	}
	if !lost {
		rs.saveFinal(st, func(r *Run) { r.Status, r.Error, r.FinishedAt = status, msg, &now })
	}
	st.mu.Lock()
	for ch := range st.subs {
		close(ch)
	}
	st.subs = map[chan runEvent]struct{}{}
	r := st.run
	lost = st.lost
	st.mu.Unlock()
	rs.mu.Lock()
	delete(rs.local, id)
	rs.mu.Unlock()
	if lost {
		// Whoever failed or deleted it has the last word (and notified).
		rs.metrics.runFinished(r.Type, StatusFailed)
		return StatusFailed, "deleted or taken over while working"
	}
	rs.metrics.runFinished(r.Type, status)
	if rs.finished != nil {
		rs.finished(r)
	}
	return status, msg
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
// unpacking it from a shared store when another replica wrote it, or
// pushed it since this replica's copy (version is the run's
// OutputVersion).
func (rs *runs) ensureOutput(ctx context.Context, id string, version int) (string, error) {
	dir := rs.outputDir(id)
	if !rs.store.shared() {
		if _, err := os.Stat(dir); err != nil {
			return "", errNotFound
		}
		return dir, nil
	}
	release := rs.dirs.lock(id)
	defer release()
	if _, err := os.Stat(dir); err == nil {
		// No version file: the copy the run itself wrote (version 0).
		have := 0
		if b, err := os.ReadFile(rs.versionFile(id)); err == nil {
			have, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if have >= version {
			now := time.Now()
			if os.Chtimes(rs.versionFile(id), now, now) != nil {
				rs.markOutput(id, have)
			}
			return dir, nil
		}
	}
	data, err := rs.store.getBlob(ctx, id, outputBlob)
	if err != nil {
		return "", err
	}
	tmp, old := dir+".unpack", dir+".old"
	_ = os.RemoveAll(tmp)
	if err := unpackDir(data, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	_ = os.RemoveAll(old)
	rs.markOutput(id, version)
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

// list returns up to q.Limit runs q selects and keep accepts, newest
// first, reading the store a page at a time.
func (rs *runs) list(q runQuery, keep func(*Run) bool) []Run {
	ctx, cancel := bg()
	defer cancel()
	want := q.Limit
	q.Limit = max(2*want, 200)
	var out []Run
	for range 50 {
		page, err := rs.store.listRuns(ctx, q)
		if err != nil {
			rs.log.Error("listing runs", "error", err)
			return out
		}
		for i := range page {
			if keep(&page[i]) {
				page[i].Spec = nil // lists stay small; the run itself has it
				out = append(out, page[i])
				if len(out) == want {
					return out
				}
			}
		}
		if len(page) < q.Limit {
			break
		}
		q.Before, q.BeforeID = page[len(page)-1].CreatedAt, page[len(page)-1].ID
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

// remove deletes a finished run and its files (not while it's pushed).
func (rs *runs) remove(id string) error {
	r, ok := rs.get(id)
	if !ok {
		return os.ErrNotExist
	}
	if !r.finished() {
		return errUnfinished
	}
	ctx, cancel := bg()
	defer cancel()
	_, unlock, ok, err := rs.store.lockRun(ctx, id)
	if errors.Is(err, errNotFound) {
		return os.ErrNotExist
	}
	if err != nil {
		return err
	}
	if !ok {
		return errPushing
	}
	defer unlock()
	if err := rs.store.deleteRun(ctx, id); err != nil {
		return err
	}
	release := rs.dirs.lock(id)
	defer release()
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
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The run first: its lines are stored before its final status, so
		// a finished run's log read after it is complete.
		sctx, cancel := bg()
		r, gerr := rs.store.getRun(sctx, id)
		lines, err := rs.store.readLog(sctx, id, seq)
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
		if errors.Is(gerr, errNotFound) {
			return // deleted
		}
		if gerr != nil {
			// A database blip doesn't end the stream; half a minute of
			// them does (the browser reconnects).
			if failures++; failures > 30 {
				return
			}
			continue
		}
		failures = 0
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ids, err := rs.store.pruneRuns(ctx, time.Now().Add(-rs.retention))
	if err != nil {
		rs.log.Warn("pruning runs", "error", err)
	}
	for _, id := range ids {
		release := rs.dirs.lock(id)
		_ = os.RemoveAll(rs.runDir(id))
		release()
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
			st.interrupted = true
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
