package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// Run is one piece of work: a scan or a migration.
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

// runEvent is what subscribers of a run receive.
type runEvent struct {
	Log *LogLine
	Run *Run
}

func (r *Run) finished() bool {
	return r.Status == StatusSucceeded || r.Status == StatusFailed || r.Status == StatusCanceled
}

// runs keeps runs on disk (dataDir/runs/<id>) and works on them.
type runs struct {
	dir       string
	timeout   time.Duration
	retention time.Duration
	sem       chan struct{}
	log       *slog.Logger
	metrics   *metrics
	// finished is told about every run that finishes (notifications).
	finished func(Run)

	mu     sync.Mutex
	byID   map[string]*runState
	closed bool
	wg     sync.WaitGroup
}

type runState struct {
	mu     sync.Mutex
	run    Run
	lines  []LogLine
	subs   map[chan runEvent]struct{}
	cancel context.CancelFunc
	file   *os.File
}

func newRuns(dir string, concurrency int, timeout, retention time.Duration, log *slog.Logger, m *metrics) (*runs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rs := &runs{dir: dir, timeout: timeout, retention: retention, sem: make(chan struct{}, concurrency), log: log,
		metrics: m, byID: map[string]*runState{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "run.json"))
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) != nil || r.ID != e.Name() {
			continue
		}
		st := &runState{run: r, subs: map[chan runEvent]struct{}{}}
		if !r.finished() {
			// The server stopped while it worked: it won't finish.
			now := time.Now().UTC()
			st.run.Status, st.run.Error, st.run.FinishedAt = StatusFailed, "the server restarted while the run worked", &now
			_ = rs.save(&st.run)
		}
		rs.byID[r.ID] = st
	}
	return rs, nil
}

func (rs *runs) runDir(id string) string { return filepath.Join(rs.dir, id) }

// outputDir holds a run's generated files.
func (rs *runs) outputDir(id string) string { return filepath.Join(rs.dir, id, "output") }

func (rs *runs) save(r *Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := rs.runDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "run.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "run.json"))
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
	line := LogLine{Time: time.Now().UTC(), Level: level, Message: fmt.Sprintf(format, args...)}
	st := rc.st
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lines = append(st.lines, line)
	if st.file != nil {
		b, _ := json.Marshal(line)
		_, _ = st.file.Write(append(b, '\n'))
	}
	for ch := range st.subs {
		select {
		case ch <- runEvent{Log: &line}:
		default: // a slow subscriber misses lines rather than blocking the run
		}
	}
}

// Info logs progress, the way pipeline.Options.Log is called.
func (rc *runContext) Info(format string, args ...any) { rc.logf("info", format, args...) }
func (rc *runContext) Warn(format string, args ...any) { rc.logf("warn", format, args...) }

func (rc *runContext) update(fn func(*Run)) {
	rc.rs.update(rc.st, fn)
}

func (rs *runs) update(st *runState, fn func(*Run)) {
	st.mu.Lock()
	fn(&st.run)
	r := st.run
	if err := rs.save(&r); err != nil {
		rs.log.Error("saving run", "run", r.ID, "error", err)
	}
	for ch := range st.subs {
		rr := r
		select {
		case ch <- runEvent{Run: &rr}:
		default:
		}
	}
	st.mu.Unlock()
}

// start records a run and does work in the background. work's error fails
// the run; a canceled context cancels it.
func (rs *runs) start(r Run, work func(context.Context, *runContext) error) (*Run, error) {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil, errors.New("the server is shutting down")
	}
	r.ID = newRunID()
	r.Status = StatusQueued
	r.CreatedAt = time.Now().UTC()
	if err := rs.save(&r); err != nil {
		rs.mu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(rs.runDir(r.ID), "log.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		rs.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rs.timeout)
	st := &runState{run: r, subs: map[chan runEvent]struct{}{}, cancel: cancel, file: f}
	rs.byID[r.ID] = st
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
					rs.log.Error("run panicked", "run", st.run.ID, "panic", p)
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return work(ctx, rc)
		}()
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
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
	rc.update(func(r *Run) { r.Status, r.Error, r.FinishedAt = status, msg, &now })
	st := rc.st
	st.mu.Lock()
	if st.file != nil {
		_ = st.file.Close()
		st.file = nil
	}
	for ch := range st.subs {
		close(ch)
	}
	st.subs = map[chan runEvent]struct{}{}
	st.mu.Unlock()
	rs.metrics.runFinished(rc.st.run.Type, status)
	if rs.finished != nil {
		st.mu.Lock()
		r := st.run
		st.mu.Unlock()
		rs.finished(r)
	}
}

// get returns a copy of a run.
func (rs *runs) get(id string) (*Run, bool) {
	rs.mu.Lock()
	st, ok := rs.byID[id]
	rs.mu.Unlock()
	if !ok {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.run
	return &r, true
}

func (rs *runs) state(id string) (*runState, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	st, ok := rs.byID[id]
	return st, ok
}

// list returns runs newest first, keeping those keep accepts.
func (rs *runs) list(keep func(*Run) bool) []Run {
	rs.mu.Lock()
	sts := make([]*runState, 0, len(rs.byID))
	for _, st := range rs.byID {
		sts = append(sts, st)
	}
	rs.mu.Unlock()
	var out []Run
	for _, st := range sts {
		st.mu.Lock()
		r := st.run
		st.mu.Unlock()
		if keep(&r) {
			r.Spec = nil // lists stay small; the run itself has it
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// cancel stops a run that hasn't finished.
func (rs *runs) cancel(id string) bool {
	st, ok := rs.state(id)
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

// remove deletes a finished run and its files.
func (rs *runs) remove(id string) error {
	st, ok := rs.state(id)
	if !ok {
		return os.ErrNotExist
	}
	st.mu.Lock()
	done := st.run.finished()
	st.mu.Unlock()
	if !done {
		return errors.New("the run hasn't finished; cancel it first")
	}
	rs.mu.Lock()
	delete(rs.byID, id)
	rs.mu.Unlock()
	return os.RemoveAll(rs.runDir(id))
}

// subscribe returns the log so far and a channel of what follows; the
// channel closes when the run finishes. done reports a finished run (no
// channel).
func (rs *runs) subscribe(id string) (lines []LogLine, ch chan runEvent, unsubscribe func(), ok bool) {
	st, found := rs.state(id)
	if !found {
		return nil, nil, nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.run.finished() {
		lines, _ := rs.readLog(id, st)
		return lines, nil, func() {}, true
	}
	lines = append([]LogLine(nil), st.lines...)
	ch = make(chan runEvent, 256)
	st.subs[ch] = struct{}{}
	return lines, ch, func() {
		st.mu.Lock()
		if _, ok := st.subs[ch]; ok {
			delete(st.subs, ch)
			close(ch)
		}
		st.mu.Unlock()
	}, true
}

// readLog returns a run's log: from memory, or from its file for runs of an
// earlier process. st.mu is held.
func (rs *runs) readLog(id string, st *runState) ([]LogLine, error) {
	if len(st.lines) > 0 {
		return append([]LogLine(nil), st.lines...), nil
	}
	f, err := os.Open(filepath.Join(rs.runDir(id), "log.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []LogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var l LogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out, sc.Err()
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
	rs.closed = true
	for _, st := range rs.byID {
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
