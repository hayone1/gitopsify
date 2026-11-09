package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hayone1/gitopsify/pkg/config"
)

// pgURL is a PostgreSQL database for tests (GITOPSIFY_TEST_POSTGRES); the
// Postgres tests skip without one. test/e2e/webui/run.sh starts one.
func pgURL(t *testing.T) string {
	u := os.Getenv("GITOPSIFY_TEST_POSTGRES")
	if u == "" {
		t.Skip("GITOPSIFY_TEST_POSTGRES isn't set")
	}
	return u
}

func testStores(t *testing.T) map[string]store {
	out := map[string]store{}
	fs, err := newFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out["file"] = fs
	if u := os.Getenv("GITOPSIFY_TEST_POSTGRES"); u != "" {
		pg, err := newPGStore(context.Background(), u, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(context.Background(), `TRUNCATE gitopsify_runs, gitopsify_audit CASCADE`); err != nil {
			t.Fatal(err)
		}
		out["postgres"] = pg
	}
	t.Cleanup(func() {
		for _, s := range out {
			_ = s.close()
		}
	})
	return out
}

func TestStores(t *testing.T) {
	ctx := context.Background()
	for name, st := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			// Runs are created with microseconds, as PostgreSQL keeps them.
			old := Run{ID: "20260101-000000-aaaaa", Type: RunScan, Status: StatusRunning, CreatedAt: time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond), Spec: []byte(`{"a":1}`)}
			now := time.Now().UTC().Truncate(time.Microsecond)
			done := Run{ID: "20260101-000001-bbbbb", Type: RunMigrate, Status: StatusSucceeded, CreatedAt: now, FinishedAt: &now}
			for _, r := range []Run{old, done} {
				if err := os.MkdirAll(filepath.Join(fileRunsDir(st), r.ID), 0o700); err != nil && fileRunsDir(st) != "" {
					t.Fatal(err)
				}
				if err := st.createRun(ctx, &r, "me"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.getRun(ctx, old.ID)
			if err != nil || string(got.Spec) != `{"a":1}` {
				t.Fatalf("get: %+v %v", got, err)
			}
			list, _ := st.listRuns(ctx, runQuery{})
			if len(list) != 2 || list[0].ID != done.ID || list[0].Spec != nil {
				t.Errorf("list (newest first): %+v", list)
			}
			if l, _ := st.listRuns(ctx, runQuery{Before: done.CreatedAt}); len(l) != 1 || l[0].ID != old.ID {
				t.Errorf("list before: %+v", l)
			}
			if l, _ := st.listRuns(ctx, runQuery{Active: true, Type: RunScan, Limit: 5}); len(l) != 1 || l[0].ID != old.ID {
				t.Errorf("list active scans: %+v", l)
			}
			if l, _ := st.listRuns(ctx, runQuery{Limit: 1}); len(l) != 1 {
				t.Errorf("list limit: %+v", l)
			}
			// Paging past runs created at the same time.
			same := []Run{{ID: "20260101-000002-ccccc", Type: RunScan, Status: StatusSucceeded, CreatedAt: done.CreatedAt, FinishedAt: &now},
				{ID: "20260101-000002-ddddd", Type: RunScan, Status: StatusSucceeded, CreatedAt: done.CreatedAt, FinishedAt: &now}}
			for _, r := range same {
				_ = os.MkdirAll(filepath.Join(fileRunsDir(st), r.ID), 0o700)
				if err := st.createRun(ctx, &r, "me"); err != nil {
					t.Fatal(err)
				}
			}
			var seen []string
			q := runQuery{Limit: 1}
			for range 10 {
				page, _ := st.listRuns(ctx, q)
				if len(page) == 0 {
					break
				}
				seen = append(seen, page[0].ID)
				q.Before, q.BeforeID = page[0].CreatedAt, page[0].ID
			}
			if strings.Join(seen, " ") != same[1].ID+" "+same[0].ID+" "+done.ID+" "+old.ID {
				t.Errorf("pages: %v", seen)
			}
			for _, r := range same {
				_ = st.deleteRun(ctx, r.ID)
			}

			// Only the owner updates a run, and only until it finishes.
			upd := old
			upd.Error = "x"
			if err := st.updateRun(ctx, &upd, "someone else"); err != errFenced {
				t.Errorf("update by another owner: %v", err)
			}
			if err := st.updateRun(ctx, &upd, "me"); err != nil {
				t.Errorf("update by the owner: %v", err)
			}
			if err := st.updateRun(ctx, &done, "me"); err != errFenced {
				t.Errorf("update of a finished run: %v", err)
			}
			if lost, err := st.heartbeat(ctx, "me", []string{old.ID, "gone"}); err != nil || len(lost) != 1 || lost[0] != "gone" {
				t.Errorf("heartbeat: %v %v", lost, err)
			}

			// Pushes are appended in one step; deleted runs aren't brought back.
			if _, err := st.addPublishes(ctx, done.ID, []Publish{{Remote: "a"}}, false); err != nil {
				t.Fatal(err)
			}
			if r, err := st.addPublishes(ctx, done.ID, []Publish{{Remote: "b"}}, true); err != nil || len(r.Publishes) != 2 || r.OutputVersion != 1 {
				t.Errorf("publishes: %+v %v", r, err)
			}
			// A retry of a write that was stored changes nothing.
			if r, err := st.addPublishes(ctx, done.ID, []Publish{{Remote: "b"}}, true); err != nil || len(r.Publishes) != 2 || r.OutputVersion != 1 {
				t.Errorf("publishes retried: %+v %v", r, err)
			}
			if _, err := st.addPublishes(ctx, "gone", []Publish{{Remote: "a"}}, false); err != errNotFound {
				t.Errorf("publishes of a missing run: %v", err)
			}
			_, unlock, ok, err := st.lockRun(ctx, done.ID)
			if err != nil || !ok {
				t.Fatalf("lock: %v %v", ok, err)
			}
			if _, _, ok2, _ := st.lockRun(ctx, done.ID); ok2 {
				t.Error("a held lock was taken again")
			}
			// Pruning leaves a locked run alone.
			if ids, _ := st.pruneRuns(ctx, time.Now().Add(time.Hour)); len(ids) != 0 {
				t.Errorf("pruned a locked run: %v", ids)
			}
			unlock()
			unlock() // twice is fine
			if _, unlock2, ok2, _ := st.lockRun(ctx, done.ID); !ok2 {
				t.Error("a released lock couldn't be taken")
			} else {
				unlock2()
			}
			if _, _, _, err := st.lockRun(ctx, "gone"); err != errNotFound {
				t.Errorf("lock of a missing run: %v", err)
			}
			for i, m := range []string{"one", "two", "three"} {
				if err := st.appendLog(ctx, old.ID, i, LogLine{Time: now, Level: "info", Message: m}); err != nil {
					t.Fatal(err)
				}
			}
			lines, _ := st.readLog(ctx, old.ID, 1)
			if len(lines) != 2 || lines[0].Message != "two" {
				t.Errorf("log from 1: %+v", lines)
			}
			if err := st.putBlob(ctx, old.ID, "scan.json", []byte("{}")); err != nil {
				t.Fatal(err)
			}
			if b, err := st.getBlob(ctx, old.ID, "scan.json"); err != nil || string(b) != "{}" {
				t.Errorf("blob: %q %v", b, err)
			}
			if _, err := st.getBlob(ctx, old.ID, "nope"); err != errNotFound {
				t.Errorf("missing blob: %v", err)
			}
			_ = st.requestCancel(ctx, old.ID)
			if ids, _ := st.cancelRequested(ctx, []string{old.ID, done.ID}); len(ids) != 1 || ids[0] != old.ID {
				t.Errorf("cancel requests: %v", ids)
			}
			// Reaping: never the reaper's own runs; others' once stale.
			fail := func(r *Run) {
				now := time.Now().UTC()
				r.Status, r.FinishedAt = StatusFailed, &now
			}
			if reaped, _ := st.reapRuns(ctx, []string{old.ID}, 0, fail); len(reaped) != 0 {
				t.Errorf("reaped a run the replica works on: %+v", reaped)
			}
			if st.shared() { // one replica: other runs are a previous process's
				if reaped, _ := st.reapRuns(ctx, nil, time.Hour, fail); len(reaped) != 0 {
					t.Errorf("reaped a fresh run: %+v", reaped)
				}
			}
			if reaped, err := st.reapRuns(ctx, nil, 0, fail); err != nil || len(reaped) != 1 || reaped[0].ID != old.ID {
				t.Errorf("reap: %+v %v", reaped, err)
			}
			if r, _ := st.getRun(ctx, old.ID); r == nil || r.Status != StatusFailed {
				t.Errorf("reaped run: %+v", r)
			}
			if err := st.updateRun(ctx, &upd, "me"); err != errFenced {
				t.Errorf("the owner updated a reaped run: %v", err)
			}
			if reaped, _ := st.reapRuns(ctx, nil, 0, fail); len(reaped) != 0 {
				t.Errorf("reaped twice: %+v", reaped)
			}
			if err := st.deleteRun(ctx, old.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.getRun(ctx, old.ID); err != errNotFound {
				t.Errorf("deleted run: %v", err)
			}
			if lines, _ := st.readLog(ctx, old.ID, 0); len(lines) != 0 {
				t.Errorf("deleted run's log: %v", lines)
			}
			if ids, err := st.pruneRuns(ctx, time.Now().Add(time.Minute)); err != nil || len(ids) != 1 || ids[0] != done.ID {
				t.Errorf("prune: %v %v", ids, err)
			}
			if l, _ := st.listRuns(ctx, runQuery{}); len(l) != 0 {
				t.Errorf("after pruning: %+v", l)
			}
			base := time.Now().UTC().Truncate(time.Millisecond)
			for i, e := range []AuditEvent{
				{Actor: "ada@acme.io", Action: "run.create", Outcome: "ok", Cluster: "dev"},
				{Actor: "bob@acme.io", Action: "access.runs", Outcome: "denied", Cluster: "prod"},
				{Actor: "ada@acme.io", Action: "run.publish", Outcome: "ok", Cluster: "dev", Detail: "100%_done"},
			} {
				e.Time = base.Add(time.Duration(i) * time.Second)
				if err := st.appendAudit(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			evs, _ := st.queryAudit(ctx, auditQuery{Actor: "ADA", Action: "run.", Limit: 10})
			if len(evs) != 2 || evs[0].Action != "run.publish" {
				t.Errorf("audit by actor and action: %+v", evs)
			}
			evs, _ = st.queryAudit(ctx, auditQuery{Before: base.Add(2 * time.Second), Limit: 1})
			if len(evs) != 1 || evs[0].Actor != "bob@acme.io" {
				t.Errorf("audit before: %+v", evs)
			}
			if evs, _ := st.queryAudit(ctx, auditQuery{Action: "%", Limit: 10}); len(evs) != 0 {
				t.Errorf("a %% in a filter matched: %+v", evs)
			}
		})
	}
}

func fileRunsDir(st store) string {
	if f, ok := st.(*fileStore); ok {
		return f.runsDir
	}
	return ""
}

func TestPackDir(t *testing.T) {
	src := t.TempDir()
	for p, c := range map[string]string{"a.yaml": "a", "apps/web/b.yaml": "b", ".git/HEAD": "ref: refs/heads/main",
		".secrets/shop/db.yaml": "password: x", "envs/prod/x.secrets.auto.tfvars": "token = 1"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		_ = os.WriteFile(filepath.Join(src, p), []byte(c), 0o644)
	}
	data, err := packDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := unpackDir(data, dst); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"a.yaml": "a", "apps/web/b.yaml": "b", ".git/HEAD": "ref: refs/heads/main"} {
		if b, _ := os.ReadFile(filepath.Join(dst, p)); string(b) != c {
			t.Errorf("%s: %q", p, b)
		}
	}
	for _, p := range []string{".secrets", "envs/prod/x.secrets.auto.tfvars"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s was archived", p)
		}
	}

	// In a repository, secret-named files it tracks are kept (a push from
	// the archive would delete them otherwise); ignored ones aren't.
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	repo := t.TempDir()
	for p, c := range map[string]string{"apps/shop/values.secrets.yaml": "sops: {}", ".secrets/kept/x.yaml": "a: 1",
		".gitignore": ".secrets/local/\n*.secrets.auto.tfvars\n", ".secrets/local/db.yaml": "password: x", "x.secrets.auto.tfvars": "t = 1"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755)
		_ = os.WriteFile(filepath.Join(repo, p), []byte(c), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	data, err = packDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	dst = filepath.Join(t.TempDir(), "repo")
	if err := unpackDir(data, dst); err != nil {
		t.Fatal(err)
	}
	// gitopsify's own plaintext files never are, tracked or not.
	for p, want := range map[string]bool{"apps/shop/values.secrets.yaml": true, ".secrets/kept/x.yaml": false, ".secrets/local/db.yaml": false, "x.secrets.auto.tfvars": false} {
		if _, err := os.Stat(filepath.Join(dst, p)); (err == nil) != want {
			t.Errorf("%s archived: %v, want %v", p, err == nil, want)
		}
	}
	// And a repository tracking them is refused before pushing.
	if err := refuseTrackedSecrets(repo, "the repository"); err == nil || !strings.Contains(err.Error(), ".secrets/kept/x.yaml") {
		t.Errorf("tracked .secrets: %v", err)
	}
	if err := refuseTrackedSecrets(t.TempDir(), "x"); err != nil {
		t.Errorf("no repository: %v", err)
	}
	// An archive that escapes the directory is refused.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if err := unpackDir(buf.Bytes(), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("an escaping archive was unpacked")
	}
}

// TestReplicas: two servers on one PostgreSQL database act as one.
func TestReplicas(t *testing.T) {
	url := pgURL(t)
	pg, err := newPGStore(context.Background(), url, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pg.pool.Exec(context.Background(), `TRUNCATE gitopsify_runs, gitopsify_audit CASCADE`)
	_ = pg.close()
	shared := func(c *config.Server) {
		c.Spec.Storage = config.ServerStorage{Type: "postgres", Postgres: &config.ServerPostgres{URL: config.SettingValue{Value: url}}}
	}
	a, b := newTestEnv(t, shared), newTestEnv(t, shared)

	// A run working on replica a, followed and canceled through replica b.
	block := make(chan struct{})
	r := fakeMigration(t, a.s, "dev", block)
	c := b.login(ada)
	req, _ := http.NewRequest(http.MethodGet, b.srv.URL+"/api/v1/runs/"+r.ID+"/events", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sawLog := false
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if code := c.do(http.MethodPost, "/api/v1/runs/"+r.ID+"/cancel", nil, nil); code != 200 {
			t.Errorf("cancel through b: %d", code)
		}
	}()
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "writing files") {
			sawLog = true
		}
		if line == "event: end" {
			break
		}
	}
	if !sawLog {
		t.Error("b's event stream missed a's log")
	}
	if got := waitRun(t, a.s, r.ID); got.Status != StatusCanceled {
		t.Errorf("a's run after a cancel through b: %s", got.Status)
	}

	// A finished migration's files, served by the other replica.
	r2 := fakeMigration(t, a.s, "dev", nil)
	waitRun(t, a.s, r2.ID)
	var files []fileEntry
	c.get("/api/v1/runs/"+r2.ID+"/files", &files)
	if len(files) != 2 {
		t.Errorf("files through b: %+v", files)
	}
	// Audit events of both replicas, in one log.
	adminC := a.login(admin)
	var evs []AuditEvent
	adminC.get("/api/v1/audit?action=run.cancel", &evs)
	if len(evs) != 1 || evs[0].Actor != "ada@acme.io" {
		t.Errorf("audit on a of b's cancel: %+v", evs)
	}
}

// TestReplicaPushes: pushes of one run through two replicas each start from
// the other's last commit, and the run keeps both.
func TestReplicaPushes(t *testing.T) {
	url := pgURL(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "--bare", "-b", "main", "gitops.git"}, {"-C", "gitops.git", "config", "http.receivepack", "true"}} {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitSrv := httptest.NewServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	defer gitSrv.Close()
	remote := gitSrv.URL + "/gitops.git"

	pg, err := newPGStore(context.Background(), url, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pg.pool.Exec(context.Background(), `TRUNCATE gitopsify_runs, gitopsify_audit CASCADE`)
	_ = pg.close()
	shared := func(c *config.Server) {
		c.Spec.Storage = config.ServerStorage{Type: "postgres", Postgres: &config.ServerPostgres{URL: config.SettingValue{Value: url}}}
		c.Spec.Git.AllowHTTP = true
	}
	a, b := newTestEnv(t, shared), newTestEnv(t, shared)
	spec, _ := json.Marshal(map[string]any{"apiVersion": "gitopsify.io/v1alpha1", "kind": "Migration", "metadata": map[string]string{"name": "shop"},
		"spec": map[string]any{"output": map[string]any{"tool": "none", "repo": map[string]any{"url": remote}}}})
	write := func(dir, content string) {
		_ = os.MkdirAll(filepath.Join(dir, "apps/web"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "apps/web/deployment.yaml"), []byte(content), 0o644)
	}
	r, err := a.s.runs.start(context.Background(), Run{Type: RunMigrate, Name: "shop", Cluster: "dev", CreatedBy: "ada@acme.io", Spec: spec},
		func(ctx context.Context, rc *runContext) error {
			write(a.s.runs.outputDir(rc.ID()), "kind: Deployment\n")
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, a.s, r.ID)
	ca, cb := a.login(ada), b.login(ada)
	var files []fileEntry
	ca.get("/api/v1/runs/"+r.ID+"/files", &files) // a's copy: version 0
	if code := cb.do(http.MethodPost, "/api/v1/runs/"+r.ID+"/publish", map[string]string{"message": "first"}, nil); code != http.StatusOK {
		t.Fatalf("push through b: %d", code)
	}
	if code := ca.do(http.MethodPost, "/api/v1/runs/"+r.ID+"/publish", map[string]string{"message": "second"}, nil); code != http.StatusOK {
		t.Fatalf("push through a after b's: %d", code)
	}
	var got Run
	ca.get("/api/v1/runs/"+r.ID, &got)
	if len(got.Publishes) != 2 || got.OutputVersion != 2 {
		t.Fatalf("publishes: %+v (version %d)", got.Publishes, got.OutputVersion)
	}
	for _, p := range got.Publishes {
		if p.Error != "" {
			t.Errorf("push: %+v", p)
		}
	}
	out, _ := exec.Command(gitPath, "-C", filepath.Join(root, "gitops.git"), "log", "--format=%s", "main").CombinedOutput()
	if !strings.Contains(string(out), "first") {
		t.Errorf("remote history: %s", out)
	}
	// Deleting waits for a push, on any replica.
	_, unlock, ok, err := b.s.store.lockRun(context.Background(), r.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if code := ca.do(http.MethodDelete, "/api/v1/runs/"+r.ID, nil, nil); code != http.StatusConflict {
		t.Errorf("delete during a push: %d", code)
	}
	unlock()
	if code := ca.do(http.MethodDelete, "/api/v1/runs/"+r.ID, nil, nil); code != http.StatusOK {
		t.Errorf("delete: %d", code)
	}
}

// sharedFiles is a file store that says it's shared, to test what replicas
// do without PostgreSQL.
type sharedFiles struct{ *fileStore }

func (sharedFiles) shared() bool { return true }

func TestReplicaCopies(t *testing.T) {
	fs, err := newFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := sharedFiles{fs}
	var notified []string
	var nmu sync.Mutex
	rs, err := newRuns(st, t.TempDir(), 2, time.Minute, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		func(r Run) { nmu.Lock(); notified = append(notified, r.ID); nmu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	defer rs.shutdown(context.Background())
	write := func(dir, content string) {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(content), 0o644)
	}
	r, err := rs.start(context.Background(), Run{Type: RunMigrate, Name: "x"}, func(ctx context.Context, rc *runContext) error {
		write(rs.outputDir(rc.ID()), "v0")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for !func() bool { got, _ := rs.get(r.ID); return got.finished() }() {
		time.Sleep(10 * time.Millisecond)
	}
	ctx := context.Background()
	read := func() string {
		dir, err := rs.ensureOutput(ctx, r.ID, func() int { got, _ := rs.get(r.ID); return got.OutputVersion }())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "a.yaml"))
		return string(b)
	}
	if got := read(); got != "v0" {
		t.Fatalf("own copy: %q", got)
	}
	// Another replica pushed: a newer archive and version.
	other := t.TempDir()
	write(other, "v1")
	data, _ := packDir(other)
	if err := st.putBlob(ctx, r.ID, outputBlob, data); err != nil {
		t.Fatal(err)
	}
	if _, err := st.addPublishes(ctx, r.ID, []Publish{{Remote: "x"}}, true); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "v1" {
		t.Errorf("after another replica's push: %q", got)
	}

	// Eviction: not while the run is locked; then idle copies go.
	_, unlock, _, _ := st.lockRun(ctx, r.ID)
	rs.evict(0)
	if _, err := os.Stat(rs.runDir(r.ID)); err != nil {
		t.Error("evicted while locked")
	}
	unlock()
	rs.evict(time.Hour)
	if _, err := os.Stat(rs.runDir(r.ID)); err != nil {
		t.Error("evicted a copy in use")
	}
	rs.evict(0)
	if _, err := os.Stat(rs.runDir(r.ID)); err == nil {
		t.Error("an idle copy stayed")
	}
	if got := read(); got != "v1" {
		t.Errorf("unpacked again: %q", got)
	}

	// A run deleted while working stops, and isn't brought back or
	// notified.
	block := make(chan struct{})
	r2, _ := rs.start(context.Background(), Run{Type: RunScan, Name: "y"}, func(ctx context.Context, rc *runContext) error {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	for !func() bool { got, _ := rs.get(r2.ID); return got.Status == StatusRunning }() {
		time.Sleep(10 * time.Millisecond)
	}
	if err := st.deleteRun(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	rs.beat(ctx, []string{r2.ID})
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		rs.mu.Lock()
		_, working := rs.local[r2.ID]
		rs.mu.Unlock()
		if !working {
			break
		}
	}
	if _, err := st.getRun(ctx, r2.ID); err != errNotFound {
		t.Errorf("a deleted run came back: %v", err)
	}
	nmu.Lock()
	defer nmu.Unlock()
	if len(notified) != 1 || notified[0] != r.ID {
		t.Errorf("notified: %v", notified)
	}
}

func TestProviderEnv(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/tmp", "GITOPSIFY_SESSION_KEY=k", "DATABASE_URL=postgres://u:p@h", "AWS_ROLE_ARN=arn",
		"AWS_WEB_IDENTITY_TOKEN_FILE=/t", "GITHUB_TOKEN=x", "LC_ALL=C", "KUBERNETES_SERVICE_HOST=10.0.0.1"}
	got := providerEnv(env, []string{"AWS_*", "GITHUB_*"}, []string{"DATABASE_URL", "GITHUB_TOKEN"})
	want := []string{"PATH=/bin", "HOME=/tmp", "AWS_ROLE_ARN=arn", "AWS_WEB_IDENTITY_TOKEN_FILE=/t", "LC_ALL=C"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("env %v, want %v", got, want)
	}
	if e := providerEnv(nil, nil, nil); e == nil {
		t.Error("an empty environment is nil (it would pass the server's)")
	}
	cfg := config.ServerSpec{Auth: config.ServerAuth{SessionKey: config.SettingValue{Env: "SK"}, OIDC: &config.ServerOIDC{ClientSecret: config.SettingValue{Env: "CS"}}},
		Git:   config.ServerGit{Credentials: []config.GitCredential{{Host: "h", Token: config.SettingValue{Env: "GT"}}}},
		Tools: []config.ServerTool{{Name: "g", Settings: map[string]config.SettingValue{"auth": {Env: "GA"}, "url": {Value: "u"}}}}}
	envs := settingEnvs(reflect.ValueOf(&cfg))
	sort.Strings(envs)
	if strings.Join(envs, ",") != "CS,GA,GT,SK" {
		t.Errorf("setting envs: %v", envs)
	}
}

// flakyStore stores a run's last update but answers with an error, once.
type flakyStore struct {
	*fileStore
	failed bool
}

func (f *flakyStore) updateRun(ctx context.Context, r *Run, owner string) error {
	err := f.fileStore.updateRun(ctx, r, owner)
	if err == nil && r.finished() && !f.failed {
		f.failed = true
		return errors.New("connection reset")
	}
	return err
}

// A final update whose answer was lost isn't taken for someone else's.
func TestSaveFinalLostAnswer(t *testing.T) {
	fs, err := newFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &flakyStore{fileStore: fs}
	var notified []Run
	var nmu sync.Mutex
	rs, err := newRuns(st, t.TempDir(), 1, time.Minute, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		func(r Run) { nmu.Lock(); notified = append(notified, r); nmu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	defer rs.shutdown(context.Background())
	r, _ := rs.start(context.Background(), Run{Type: RunScan, Name: "x"}, func(context.Context, *runContext) error { return nil })
	for start := time.Now(); time.Since(start) < 15*time.Second; time.Sleep(20 * time.Millisecond) {
		nmu.Lock()
		n := len(notified)
		nmu.Unlock()
		if n > 0 {
			break
		}
	}
	nmu.Lock()
	defer nmu.Unlock()
	if !st.failed || len(notified) != 1 || notified[0].Status != StatusSucceeded {
		t.Fatalf("failed once %v, notified %+v", st.failed, notified)
	}
	if got, _ := fs.getRun(context.Background(), r.ID); got.Status != StatusSucceeded {
		t.Errorf("stored: %+v", got)
	}
}

// A lease that can't be renewed ends the work done under it.
func TestLeaseLoss(t *testing.T) {
	url := pgURL(t)
	pg, err := newPGStore(context.Background(), url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.close()
	defer func(l time.Duration) { lockLease = l }(lockLease)
	lockLease = 2 * time.Second
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := Run{ID: "20260101-000000-lease", Type: RunScan, Status: StatusSucceeded, CreatedAt: now, FinishedAt: &now}
	_, _ = pg.pool.Exec(ctx, `DELETE FROM gitopsify_runs WHERE id = $1`, r.ID)
	if err := pg.createRun(ctx, &r, "me"); err != nil {
		t.Fatal(err)
	}
	lctx, unlock, ok, err := pg.lockRun(ctx, r.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer unlock()
	if _, err := pg.pool.Exec(ctx, `UPDATE gitopsify_runs SET lock_until = now() - interval '1 second' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lctx.Done():
		if !errors.Is(context.Cause(lctx), errLockLost) {
			t.Errorf("cause: %v", context.Cause(lctx))
		}
	case <-time.After(5 * time.Second):
		t.Error("an expired lease wasn't noticed")
	}

	// The caller's context ending doesn't end the lease: it's renewed
	// until unlock.
	_, _ = pg.pool.Exec(ctx, `UPDATE gitopsify_runs SET lock_until = NULL, lock_owner = '' WHERE id = $1`, r.ID)
	parent, cancelParent := context.WithCancel(ctx)
	_, unlock3, ok, err := pg.lockRun(parent, r.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	cancelParent()
	time.Sleep(lockLease + 500*time.Millisecond)
	if _, _, ok, _ := pg.lockRun(ctx, r.ID); ok {
		t.Error("the lease ran out after the caller's context ended")
	}
	unlock3()

	// The database unreachable: lost half a lease after the last renewal.
	pg2, err := newPGStore(ctx, url, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pg.pool.Exec(ctx, `UPDATE gitopsify_runs SET lock_until = NULL, lock_owner = '' WHERE id = $1`, r.ID)
	lctx2, unlock2, ok, err := pg2.lockRun(ctx, r.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	pg2.pool.Close()
	select {
	case <-lctx2.Done():
		if !errors.Is(context.Cause(lctx2), errLockLost) {
			t.Errorf("cause: %v", context.Cause(lctx2))
		}
	case <-time.After(2 * time.Second):
		t.Error("an unreachable database wasn't noticed in time")
	}
	done := make(chan struct{})
	go func() { unlock2(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("unlock hung")
	}
}

// Runs started while the server shuts down either start before it (and it
// stops them) or are refused; none keeps working after shutdown returned.
func TestStartDuringShutdown(t *testing.T) {
	for i := 0; i < 30; i++ {
		fs, err := newFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rs, err := newRuns(fs, t.TempDir(), 2, time.Minute, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var ids []string
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := rs.start(context.Background(), Run{Type: RunScan, Name: "x"}, func(ctx context.Context, _ *runContext) error {
					<-ctx.Done()
					return ctx.Err()
				})
				switch {
				case err == nil:
					mu.Lock()
					ids = append(ids, r.ID)
					mu.Unlock()
				case !errors.Is(err, errShuttingDown):
					t.Error(err)
				}
			}()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		rs.shutdown(ctx)
		if ctx.Err() != nil {
			t.Fatal("shutdown waited for a run it didn't stop")
		}
		cancel()
		wg.Wait()
		for _, id := range ids {
			got, ok := rs.get(id)
			if !ok || !got.finished() || got.Status != StatusFailed || !strings.HasPrefix(got.Error, "interrupted") {
				t.Fatalf("run %s after shutdown: %+v", id, got)
			}
		}
	}
}

// A schema error (a database user that may not create tables) is reported,
// without hanging, and leaves the schema lock free for other replicas.
func TestPGSchemaError(t *testing.T) {
	u := pgURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	// A fresh database the limited role can't create tables in.
	for _, q := range []string{`DROP DATABASE IF EXISTS gitopsify_limited`, `DROP ROLE IF EXISTS gitopsify_limited`,
		`CREATE ROLE gitopsify_limited LOGIN PASSWORD 'pw'`, `CREATE DATABASE gitopsify_limited`} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	pu.User = url.UserPassword("gitopsify_limited", "pw")
	pu.Path = "/gitopsify_limited"
	done := make(chan error, 1)
	go func() {
		st, err := newPGStore(ctx, pu.String(), 2)
		if err == nil {
			_ = st.close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "creating the tables") {
			t.Fatalf("got %v, want a schema error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("newPGStore hung on a schema error")
	}
	// The lock is free: the real store opens at once.
	octx, ocancel := context.WithTimeout(ctx, 10*time.Second)
	defer ocancel()
	st, err := newPGStore(octx, u, 2)
	if err != nil {
		t.Fatalf("the schema lock stayed held: %v", err)
	}
	_ = st.close()
}

// A NUL character in a run (its spec, its error) or a log line doesn't break
// the run list or the log.
func TestStoresNUL(t *testing.T) {
	for name, st := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := &Run{ID: "nul-" + name, Type: RunScan, Name: "x\x00y", Status: StatusQueued, CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
				Spec: json.RawMessage(`{"metadata":{"name":"a\u0000b"}}`)}
			if err := st.createRun(ctx, r, "me"); err != nil {
				t.Fatal(err)
			}
			list, err := st.listRuns(ctx, runQuery{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, l := range list {
				found = found || l.ID == r.ID
			}
			if !found {
				t.Errorf("the run isn't listed: %v", list)
			}
			line := LogLine{Time: time.Now().UTC(), Level: "info", Message: logText("a\x00b")}
			if err := st.appendLog(ctx, r.ID, 0, line); err != nil {
				t.Fatal(err)
			}
			if lines, err := st.readLog(ctx, r.ID, 0); err != nil || len(lines) != 1 {
				t.Errorf("log: %v %v", lines, err)
			}
		})
	}
}
