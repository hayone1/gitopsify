package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
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
				if err := st.saveRun(ctx, &r, "me"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.getRun(ctx, old.ID)
			if err != nil || string(got.Spec) != `{"a":1}` {
				t.Fatalf("get: %+v %v", got, err)
			}
			list, _ := st.listRuns(ctx)
			if len(list) != 2 || list[0].ID != done.ID {
				t.Errorf("list (newest first): %+v", list)
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
			if stale, _ := st.staleRuns(ctx, time.Now().Add(time.Minute)); len(stale) != 1 || stale[0].ID != old.ID {
				t.Errorf("stale: %+v", stale)
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
	for p, c := range map[string]string{"a.yaml": "a", "apps/web/b.yaml": "b", ".git/HEAD": "ref: refs/heads/main"} {
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
