package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// errNotFound is returned for runs and blobs that don't exist.
var errNotFound = errors.New("not found")

// errFenced is returned to the owner of a run when it may no longer write
// it: the run was deleted, or another replica failed it as abandoned.
var errFenced = errors.New("the run was taken over")

// runQuery selects runs, newest first (by creation, then ID).
type runQuery struct {
	// Before and BeforeID page: runs after the run created at Before with
	// ID BeforeID in that order (zero: the newest; no ID: created before).
	Before                           time.Time
	BeforeID                         string
	Type, Cluster, Status, CreatedBy string
	// Active keeps unfinished runs only.
	Active bool
	Limit  int
}

func (q runQuery) matches(r *Run) bool {
	return (q.Before.IsZero() || r.CreatedAt.Before(q.Before) || (q.BeforeID != "" && r.CreatedAt.Equal(q.Before) && r.ID < q.BeforeID)) &&
		(q.Type == "" || r.Type == q.Type) && (q.Cluster == "" || r.Cluster == q.Cluster) &&
		(q.Status == "" || r.Status == q.Status) && (q.CreatedBy == "" || r.CreatedBy == q.CreatedBy) &&
		(!q.Active || !r.finished())
}

// store keeps what outlives a request: runs, their logs and outputs, and
// the audit log. The file store keeps them in the data directory (one
// replica); the Postgres store shares them between replicas.
//
// A run belongs to the replica that created it (its owner) until it
// finishes: only the owner updates it, and only while it's unfinished and
// still its own, so a replica that failed an abandoned run, or someone who
// deleted one, can't be overwritten by a late owner.
type store interface {
	createRun(ctx context.Context, r *Run, owner string) error
	// updateRun saves an unfinished run its owner works on (errFenced
	// otherwise).
	updateRun(ctx context.Context, r *Run, owner string) error
	getRun(ctx context.Context, id string) (*Run, error)
	// listRuns returns the runs q selects, newest first, without Spec.
	listRuns(ctx context.Context, q runQuery) ([]Run, error)
	deleteRun(ctx context.Context, id string) error
	// addPublishes records pushes of a finished run in one step; bump
	// moves its output version on (the archive changed). It returns the
	// run as saved.
	addPublishes(ctx context.Context, id string, ps []Publish, bump bool) (*Run, error)
	// lockRun takes a run's lock (held while pushing or deleting it),
	// across replicas; ok is false when someone else holds it, and the
	// error errNotFound when the run doesn't exist. The returned context,
	// derived from ctx, ends if the lock is lost before unlock.
	lockRun(ctx context.Context, id string) (lctx context.Context, unlock func(), ok bool, err error)
	// pruneRuns deletes the runs finished before before, but locked ones,
	// and returns them.
	pruneRuns(ctx context.Context, before time.Time) ([]string, error)
	appendLog(ctx context.Context, id string, seq int, l LogLine) error
	// readLog returns the lines from seq from on.
	readLog(ctx context.Context, id string, from int) ([]LogLine, error)
	putBlob(ctx context.Context, id, name string, data []byte) error
	getBlob(ctx context.Context, id, name string) ([]byte, error)
	// requestCancel asks the replica working on a run to stop it.
	requestCancel(ctx context.Context, id string) error
	cancelRequested(ctx context.Context, ids []string) ([]string, error)
	// heartbeat records that owner still works on the runs, and returns
	// those it lost (deleted, or failed by another replica).
	heartbeat(ctx context.Context, owner string, ids []string) (lost []string, err error)
	// reapRuns fails the unfinished runs, but those in keep (this
	// replica's), that had no heartbeat for staleAfter (by the store's
	// clock), and returns them.
	reapRuns(ctx context.Context, keep []string, staleAfter time.Duration, fail func(*Run)) ([]Run, error)
	appendAudit(ctx context.Context, e AuditEvent) error
	queryAudit(ctx context.Context, q auditQuery) ([]AuditEvent, error)
	pruneAudit(ctx context.Context, before time.Time) error
	// shared reports whether other replicas see the same data.
	shared() bool
	close() error
}

// fileStore keeps everything under the data directory:
// runs/<id>/{run.json,log.jsonl,<blobs>} and audit/audit-<day>.jsonl.
type fileStore struct {
	runsDir, auditDir string

	// wmu serializes writes of run.json (read, change, write).
	wmu      sync.Mutex
	mu       sync.Mutex
	runs     map[string]Run
	owners   map[string]string
	cancels  map[string]bool
	auditDay string
	audit    *os.File
	locks    keyLocks
}

func newFileStore(dataDir string) (*fileStore, error) {
	f := &fileStore{runsDir: filepath.Join(dataDir, "runs"), auditDir: filepath.Join(dataDir, "audit"),
		runs: map[string]Run{}, owners: map[string]string{}, cancels: map[string]bool{}}
	for _, d := range []string{f.runsDir, f.auditDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(f.runsDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(f.runsDir, e.Name(), "run.json"))
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) == nil && r.ID == e.Name() {
			f.runs[r.ID] = r
		}
	}
	return f, nil
}

func (f *fileStore) shared() bool { return false }

func (f *fileStore) createRun(_ context.Context, r *Run, owner string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	f.owners[r.ID] = owner
	f.mu.Unlock()
	return f.write(r)
}

func (f *fileStore) updateRun(_ context.Context, r *Run, owner string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	cur, ok := f.runs[r.ID]
	mine := f.owners[r.ID] == owner
	f.mu.Unlock()
	if !ok || !mine || cur.finished() {
		return errFenced
	}
	return f.write(r)
}

func (f *fileStore) addPublishes(_ context.Context, id string, ps []Publish, bump bool) (*Run, error) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	r, ok := f.runs[id]
	f.mu.Unlock()
	if !ok {
		return nil, errNotFound
	}
	if !addNewPublishes(&r, ps) {
		return &r, nil
	}
	if bump {
		r.OutputVersion++
	}
	return &r, f.write(&r)
}

func (f *fileStore) lockRun(ctx context.Context, id string) (context.Context, func(), bool, error) {
	f.mu.Lock()
	_, ok := f.runs[id]
	f.mu.Unlock()
	if !ok {
		return nil, nil, false, errNotFound
	}
	if !f.locks.TryLock(id) {
		return nil, nil, false, nil
	}
	var once sync.Once
	return ctx, func() { once.Do(func() { f.locks.Unlock(id) }) }, true, nil
}

func (f *fileStore) pruneRuns(ctx context.Context, before time.Time) ([]string, error) {
	f.mu.Lock()
	var ids []string
	for id, r := range f.runs {
		if r.finished() && r.FinishedAt != nil && r.FinishedAt.Before(before) {
			ids = append(ids, id)
		}
	}
	f.mu.Unlock()
	var pruned []string
	for _, id := range ids {
		if !f.locks.TryLock(id) {
			continue // being pushed: next time
		}
		err := f.deleteRun(ctx, id)
		f.locks.Unlock(id)
		if err != nil {
			return pruned, err
		}
		pruned = append(pruned, id)
	}
	return pruned, nil
}

// write saves run.json (callers hold wmu).
func (f *fileStore) write(r *Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(f.runsDir, r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".run-*.json")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "run.json"))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	f.mu.Lock()
	f.runs[r.ID] = *r
	f.mu.Unlock()
	return nil
}

func (f *fileStore) getRun(_ context.Context, id string) (*Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, errNotFound
	}
	return &r, nil
}

func (f *fileStore) listRuns(_ context.Context, q runQuery) ([]Run, error) {
	f.mu.Lock()
	out := make([]Run, 0, len(f.runs))
	for _, r := range f.runs {
		if q.matches(&r) {
			r.Spec = nil
			out = append(out, r)
		}
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fileStore) deleteRun(_ context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return errNotFound
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	delete(f.runs, id)
	delete(f.owners, id)
	delete(f.cancels, id)
	f.mu.Unlock()
	return os.RemoveAll(filepath.Join(f.runsDir, id))
}

func (f *fileStore) appendLog(_ context.Context, id string, _ int, l LogLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(f.runsDir, id, "log.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(b, '\n'))
	return err
}

func (f *fileStore) readLog(_ context.Context, id string, from int) ([]LogLine, error) {
	file, err := os.Open(filepath.Join(f.runsDir, id, "log.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []LogLine
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for i := 0; sc.Scan(); i++ {
		if i < from {
			continue
		}
		var l LogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func (f *fileStore) putBlob(_ context.Context, id, name string, data []byte) error {
	return os.WriteFile(filepath.Join(f.runsDir, id, name), data, 0o600)
}

func (f *fileStore) getBlob(_ context.Context, id, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(f.runsDir, id, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	return b, err
}

func (f *fileStore) requestCancel(_ context.Context, id string) error {
	f.mu.Lock()
	f.cancels[id] = true
	f.mu.Unlock()
	return nil
}

func (f *fileStore) cancelRequested(_ context.Context, ids []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range ids {
		if f.cancels[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fileStore) heartbeat(_ context.Context, _ string, ids []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lost []string
	for _, id := range ids {
		if _, ok := f.runs[id]; !ok {
			lost = append(lost, id)
		}
	}
	return lost, nil
}

// reapRuns: with one replica, every unfinished run the process doesn't
// work on (the process before a restart worked on it) is abandoned,
// however recent.
func (f *fileStore) reapRuns(_ context.Context, keep []string, _ time.Duration, fail func(*Run)) ([]Run, error) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	var out []Run
	for id, r := range f.runs {
		if !r.finished() && !contains(keep, id) {
			fail(&r)
			out = append(out, r)
		}
	}
	f.mu.Unlock()
	for i := range out {
		if err := f.write(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (f *fileStore) appendAudit(_ context.Context, e AuditEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	day := e.Time.UTC().Format("2006-01-02")
	if f.audit == nil || f.auditDay != day {
		if f.audit != nil {
			_ = f.audit.Close()
		}
		file, err := os.OpenFile(filepath.Join(f.auditDir, "audit-"+day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			f.audit = nil
			return err
		}
		f.audit, f.auditDay = file, day
	}
	_, err = f.audit.Write(append(b, '\n'))
	return err
}

func (f *fileStore) queryAudit(_ context.Context, q auditQuery) ([]AuditEvent, error) {
	files, err := filepath.Glob(filepath.Join(f.auditDir, "audit-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	var out []AuditEvent
	for _, file := range files {
		evs, err := readAudit(file)
		if err != nil {
			return nil, err
		}
		for i := len(evs) - 1; i >= 0; i-- {
			if q.matches(evs[i]) {
				out = append(out, evs[i])
				if len(out) == q.Limit {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

func (f *fileStore) pruneAudit(_ context.Context, before time.Time) error {
	files, _ := filepath.Glob(filepath.Join(f.auditDir, "audit-*.jsonl"))
	cutoff := before.UTC().Format("2006-01-02")
	for _, file := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "audit-"), ".jsonl")
		if day < cutoff {
			_ = os.Remove(file)
		}
	}
	return nil
}

func (f *fileStore) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.audit != nil {
		err := f.audit.Close()
		f.audit = nil
		return err
	}
	return nil
}

func readAudit(path string) ([]AuditEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []AuditEvent
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// --- output archives (for stores shared between replicas) --------------------

// addNewPublishes appends the pushes a run doesn't have yet (a retried
// write may have been stored already), and reports whether there were any.
func addNewPublishes(r *Run, ps []Publish) bool {
	added := false
	for _, p := range ps {
		dup := false
		for _, q := range r.Publishes {
			if q.At.Equal(p.At) && q.Remote == p.Remote && q.Dir == p.Dir && q.By == p.By {
				dup = true
				break
			}
		}
		if !dup {
			r.Publishes = append(r.Publishes, p)
			added = true
		}
	}
	return added
}

// trackedFiles adds the files the repository in dir tracks, prefixed with
// prefix (dir's path in the archive).
func trackedFiles(dir, prefix string, into map[string]bool) error {
	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "-C", dir, "ls-files", "-z")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %w", err)
	}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		if prefix != "." && prefix != "" {
			f = prefix + "/" + f
		}
		into[f] = true
	}
	return nil
}

func hasTracked(tracked map[string]bool, dirPrefix string) bool {
	for f := range tracked {
		if strings.HasPrefix(f, dirPrefix) {
			return true
		}
	}
	return false
}

// packDir archives a directory, everything in it (Git history included).
func packDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Files a repository in the output tracks, by path from dir: secret
	// files gitopsify wrote (git-ignored) stay out of the archive, but
	// tracked ones (a SOPS-encrypted values.secrets.yaml) must stay, or a
	// push from the archive would delete them.
	tracked := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if fi, err := os.Lstat(filepath.Join(p, ".git")); err == nil && fi.IsDir() {
				if err := trackedFiles(p, filepath.ToSlash(rel), tracked); err != nil {
					slog.Warn("archiving a run's output: secret-named files the repository may track are left out", "dir", rel, "error", err)
				}
			}
		}
		if rel == "." || !(d.IsDir() || d.Type().IsRegular()) {
			return nil
		}
		slash := filepath.ToSlash(rel)
		inGit := slash == ".git" || strings.HasPrefix(slash, ".git/") || strings.Contains(slash, "/.git/") || strings.HasSuffix(slash, "/.git")
		// Kept out of the database (and its backups): what gitopsify writes
		// in clear always; other secret-named files unless a repository
		// tracks them (a SOPS-encrypted values.secrets.yaml).
		if !inGit && (ownSecretFile(slash) || (secretFile(slash) && !tracked[slash] && !(d.IsDir() && hasTracked(tracked, slash+"/")))) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unpackDir extracts an archive packDir made into dir, refusing entries that
// would land outside it.
func unpackDir(data []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return fmt.Errorf("archive entry %q escapes the directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, hdr.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}
