package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			old := Run{ID: "20260101-000000-aaaaa", Type: RunScan, Status: StatusRunning, CreatedAt: time.Now().Add(-time.Hour).UTC(), Spec: []byte(`{"a":1}`)}
			now := time.Now().UTC()
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
			if _, err := st.addPublishes(ctx, "gone", []Publish{{Remote: "a"}}, false); err != errNotFound {
				t.Errorf("publishes of a missing run: %v", err)
			}
			unlock, ok, err := st.tryLock(ctx, "run:"+done.ID)
			if err != nil || !ok {
				t.Fatalf("lock: %v %v", ok, err)
			}
			if _, ok2, _ := st.tryLock(ctx, "run:"+done.ID); ok2 {
				t.Error("a held lock was taken again")
			}
			unlock()
			if unlock2, ok2, _ := st.tryLock(ctx, "run:"+done.ID); !ok2 {
				t.Error("a released lock couldn't be taken")
			} else {
				unlock2()
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
			if reaped, _ := st.reapRuns(ctx, "me", 0, fail); len(reaped) != 0 {
				t.Errorf("reaped its own: %+v", reaped)
			}
			if st.shared() { // one replica: the other owner's runs are a previous process's
				if reaped, _ := st.reapRuns(ctx, "other", time.Hour, fail); len(reaped) != 0 {
					t.Errorf("reaped a fresh run: %+v", reaped)
				}
			}
			if reaped, err := st.reapRuns(ctx, "other", 0, fail); err != nil || len(reaped) != 1 || reaped[0].ID != old.ID {
				t.Errorf("reap: %+v %v", reaped, err)
			}
			if r, _ := st.getRun(ctx, old.ID); r == nil || r.Status != StatusFailed {
				t.Errorf("reaped run: %+v", r)
			}
			if err := st.updateRun(ctx, &upd, "me"); err != errFenced {
				t.Errorf("the owner updated a reaped run: %v", err)
			}
			if reaped, _ := st.reapRuns(ctx, "other", 0, fail); len(reaped) != 0 {
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
	r, err := a.s.runs.start(Run{Type: RunMigrate, Name: "shop", Cluster: "dev", CreatedBy: "ada@acme.io", Spec: spec},
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
	unlock, ok, err := b.s.store.tryLock(context.Background(), "run:"+r.ID)
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
