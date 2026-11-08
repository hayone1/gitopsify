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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// errNotFound is returned for runs and blobs that don't exist.
var errNotFound = errors.New("not found")

// store keeps what outlives a request: runs, their logs and outputs, and
// the audit log. The file store keeps them in the data directory (one
// replica); the Postgres store shares them between replicas.
type store interface {
	saveRun(ctx context.Context, r *Run, owner string) error
	getRun(ctx context.Context, id string) (*Run, error)
	// listRuns returns every run, newest first; Spec may be left out.
	listRuns(ctx context.Context) ([]Run, error)
	deleteRun(ctx context.Context, id string) error
	appendLog(ctx context.Context, id string, seq int, l LogLine) error
	// readLog returns the lines from seq from on.
	readLog(ctx context.Context, id string, from int) ([]LogLine, error)
	putBlob(ctx context.Context, id, name string, data []byte) error
	getBlob(ctx context.Context, id, name string) ([]byte, error)
	// requestCancel asks the replica working on a run to stop it.
	requestCancel(ctx context.Context, id string) error
	cancelRequested(ctx context.Context, ids []string) ([]string, error)
	// heartbeat records that this replica still works on the runs.
	heartbeat(ctx context.Context, ids []string) error
	// staleRuns returns unfinished runs nobody has worked on since before.
	staleRuns(ctx context.Context, before time.Time) ([]Run, error)
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

	mu       sync.Mutex
	runs     map[string]Run
	cancels  map[string]bool
	auditDay string
	audit    *os.File
}

func newFileStore(dataDir string) (*fileStore, error) {
	f := &fileStore{runsDir: filepath.Join(dataDir, "runs"), auditDir: filepath.Join(dataDir, "audit"),
		runs: map[string]Run{}, cancels: map[string]bool{}}
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

func (f *fileStore) saveRun(_ context.Context, r *Run, _ string) error {
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

func (f *fileStore) listRuns(context.Context) ([]Run, error) {
	f.mu.Lock()
	out := make([]Run, 0, len(f.runs))
	for _, r := range f.runs {
		r.Spec = nil
		out = append(out, r)
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fileStore) deleteRun(_ context.Context, id string) error {
	f.mu.Lock()
	delete(f.runs, id)
	delete(f.cancels, id)
	f.mu.Unlock()
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return errNotFound
	}
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

func (f *fileStore) heartbeat(context.Context, []string) error { return nil }

// staleRuns: with one replica, every unfinished run the process doesn't
// know (it started before a restart) is stale.
func (f *fileStore) staleRuns(context.Context, time.Time) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Run
	for _, r := range f.runs {
		if !r.finished() {
			out = append(out, r)
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

// packDir archives a directory, everything in it (Git history included).
func packDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." || !(d.IsDir() || d.Type().IsRegular()) {
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
