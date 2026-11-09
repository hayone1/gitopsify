package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-s.stop: // stopping: send new requests to other replicas
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("stopping\n"))
		default:
			_, _ = w.Write([]byte("ok\n"))
		}
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.metricsKey != "" && !constantEq(r.Header.Get("Authorization"), "Bearer "+s.metricsKey) {
			writeError(w, http.StatusUnauthorized, "send the metrics token")
			return
		}
		s.metrics.ServeHTTP(w, r)
	})

	s.api(mux, "GET /api/v1/info", "public", s.getInfo)
	s.api(mux, "GET /api/v1/auth/login", "public", s.login)
	s.api(mux, "GET /api/v1/auth/callback", "public", s.callback)
	s.api(mux, "GET /api/v1/auth/local", "public", s.localLogin)
	s.api(mux, "POST /api/v1/auth/logout", "", s.logout)
	s.api(mux, "GET /api/v1/me", "", s.getMe)

	s.api(mux, "GET /api/v1/clusters", config.RoleViewer, s.listClusters)
	s.api(mux, "GET /api/v1/clusters/{name}/check", config.RoleViewer, s.clusterCheck)
	s.api(mux, "GET /api/v1/clusters/{name}/namespaces", config.RoleViewer, s.clusterNamespaces)

	s.api(mux, "GET /api/v1/migration/template", config.RoleViewer, s.migrationTemplate)
	s.api(mux, "POST /api/v1/migration/validate", config.RoleViewer, s.validateMigration)
	s.api(mux, "GET /api/v1/migration/schema", config.RoleViewer, s.migrationSchema)

	s.api(mux, "GET /api/v1/runs", config.RoleViewer, s.listRuns)
	s.api(mux, "POST /api/v1/runs", config.RoleOperator, s.createRun)
	s.api(mux, "GET /api/v1/runs/{id}", config.RoleViewer, s.getRun)
	s.api(mux, "DELETE /api/v1/runs/{id}", config.RoleViewer, s.deleteRun)
	s.api(mux, "POST /api/v1/runs/{id}/cancel", config.RoleOperator, s.cancelRun)
	s.api(mux, "POST /api/v1/runs/{id}/publish", config.RoleOperator, s.publishRun)
	s.api(mux, "GET /api/v1/runs/{id}/events", config.RoleViewer, s.runEvents)
	s.api(mux, "GET /api/v1/runs/{id}/log", config.RoleViewer, s.runLog)
	s.api(mux, "GET /api/v1/runs/{id}/scan", config.RoleViewer, s.runScan)
	s.api(mux, "GET /api/v1/runs/{id}/files", config.RoleViewer, s.runFiles)
	s.api(mux, "GET /api/v1/runs/{id}/files/{path...}", config.RoleViewer, s.runFile)
	s.api(mux, "GET /api/v1/runs/{id}/archive", config.RoleViewer, s.runArchive)

	s.api(mux, "GET /api/v1/tools", config.RoleViewer, s.listTools)
	s.api(mux, "GET /api/v1/tools/{name}/check", config.RoleViewer, s.checkTool)
	s.api(mux, "GET /api/v1/providers", config.RoleViewer, s.listProviders)
	s.api(mux, "POST /api/v1/providers", config.RoleAdmin, s.installProvider)
	s.api(mux, "GET /api/v1/export/template", config.RoleViewer, s.exportTemplate)

	s.api(mux, "GET /api/v1/audit", config.RoleAdmin, s.listAudit)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "no such API") })
	mux.Handle("/", s.ui)
	return s.middleware(mux)
}

// --- info and sign-in --------------------------------------------------------

func (s *Server) getInfo(w http.ResponseWriter, _ *http.Request, _ *reqInfo) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"version": s.version, "authMode": s.auth.mode, "local": s.local})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, _ *reqInfo) {
	if s.auth.oidc == nil {
		writeError(w, http.StatusNotFound, "this server doesn't use OIDC")
		return
	}
	if err := s.auth.startLogin(w, r); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if s.auth.oidc == nil {
		writeError(w, http.StatusNotFound, "this server doesn't use OIDC")
		return
	}
	u, redirect, err := s.auth.finishLogin(w, r)
	if err != nil {
		s.metrics.login("failed")
		s.audit.record(AuditEvent{Action: "auth.login", Outcome: "error", Detail: err.Error(), IP: ri.ip, Request: ri.id, Via: "oidc"})
		code := "failed"
		var le *loginError
		if errors.As(err, &le) {
			code = le.code
		}
		http.Redirect(w, r, "/login?error="+code, http.StatusFound)
		return
	}
	full := *u
	if _, err := s.auth.setSession(w, full); err != nil {
		// Too many groups for a cookie: keep the ones bindings use.
		u.Groups = s.policy.relevantGroups(u.Groups)
		if _, err := s.auth.setSession(w, *u); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.log.Warn("kept only the groups bindings name in the session", "user", u.Username, "groups", len(full.Groups))
	}
	role := anyRole(s.policy.grants(u, nil))
	s.metrics.login("ok")
	s.audit.record(AuditEvent{Actor: u.Username, Via: "oidc", Action: "auth.login", Outcome: "ok", Detail: "role " + firstNonEmpty(role, "none"),
		IP: ri.ip, Request: ri.id})
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (s *Server) localLogin(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if s.auth.mode != config.AuthLocal || s.auth.local == "" {
		writeError(w, http.StatusNotFound, "this server doesn't use local sign-in")
		return
	}
	// The link works once: it may sit in the browser's history.
	if !s.auth.useLocalToken(r.URL.Query().Get("token")) {
		s.metrics.login("failed")
		s.audit.record(AuditEvent{Action: "auth.login", Outcome: "denied", Detail: "wrong or used local token", IP: ri.ip, Request: ri.id, Via: "local"})
		http.Redirect(w, r, "/login?error=link", http.StatusFound)
		return
	}
	u := User{Username: "local", Name: "You", Via: "local"}
	if _, err := s.auth.setSession(w, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.login("ok")
	s.audit.record(AuditEvent{Actor: u.Username, Via: "local", Action: "auth.login", Outcome: "ok", IP: ri.ip, Request: ri.id})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	s.auth.clearSession(w)
	s.event(r, "auth.logout", "ok", "", "", "")
	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// me is who the caller is and what they may do.
type me struct {
	User       User              `json:"user"`
	Role       string            `json:"role"`
	ServerRole string            `json:"serverRole"`
	Clusters   map[string]string `json:"clusters"`
	Tools      map[string]string `json:"tools"`
	CSRF       string            `json:"csrf,omitempty"`
	AuthMode   string            `json:"authMode"`
	// CanInstall reports whether the caller may install providers.
	CanInstall bool       `json:"canInstall"`
	Version    string     `json:"version"`
	Expires    *time.Time `json:"expires,omitempty"`
}

func (s *Server) getMe(w http.ResponseWriter, _ *http.Request, ri *reqInfo) {
	m := me{User: *ri.user, Role: anyRole(ri.grants), ServerRole: serverRole(ri.grants), Clusters: map[string]string{},
		Tools: map[string]string{}, AuthMode: s.auth.mode, Version: s.version,
		CanInstall: !s.cfg.Providers.DisableInstall && atLeast(serverRole(ri.grants), config.RoleAdmin)}
	for _, t := range s.cfg.Tools {
		if role := roleOnTool(ri.grants, t.Name); role != "" {
			m.Tools[t.Name] = role
		}
	}
	if ri.session != nil {
		m.CSRF = ri.session.CSRF
		t := time.Unix(ri.session.Expires, 0).UTC()
		m.Expires = &t
	}
	if list, err := s.clusterList(); err == nil {
		for _, c := range list {
			if role := roleOn(ri.grants, c.Name); role != "" {
				m.Clusters[c.Name] = role
			}
		}
	}
	writeJSONResponse(w, http.StatusOK, m)
}

// --- clusters ------------------------------------------------------------------

func (s *Server) listClusters(w http.ResponseWriter, _ *http.Request, ri *reqInfo) {
	list, err := s.clusterList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []clusterInfo{}
	for _, c := range list {
		if role := roleOn(ri.grants, c.Name); role != "" {
			out = append(out, clusterInfo{Name: c.Name, Description: c.Description, Impersonate: c.Impersonate, Role: role})
		}
	}
	writeJSONResponse(w, http.StatusOK, out)
}

func (s *Server) clusterCheck(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	name := r.PathValue("name")
	if !s.can(w, r, ri, config.RoleViewer, name, "clusters.check") {
		return
	}
	writeJSONResponse(w, http.StatusOK, s.checkCluster(r.Context(), name, ri.user))
}

func (s *Server) clusterNamespaces(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	name := r.PathValue("name")
	if !s.can(w, r, ri, config.RoleViewer, name, "clusters.namespaces") {
		return
	}
	ns, err := s.namespaces(r.Context(), name, ri.user)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, ns)
}

// --- migrations ------------------------------------------------------------------

// migrationTemplate is a starting Migration for a cluster, with the
// defaults written out.
func (s *Server) migrationTemplate(w http.ResponseWriter, r *http.Request, _ *reqInfo) {
	cluster := r.URL.Query().Get("cluster")
	c := &config.Config{}
	c.Metadata.Name = config.NameFor(cluster)
	c.Default()
	c.Source.Access = config.Access{}
	c.Output.Dir = ""
	b, err := c.Marshal()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"migration": string(b)})
}

func (s *Server) validateMigration(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	var req runRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only for what the caller may use: no probing for names.
	if req.Type == RunExport {
		if roleOnTool(ri.grants, req.Tool) == "" {
			writeError(w, http.StatusForbidden, "you have no role on tool "+req.Tool)
			return
		}
	} else if !s.can(w, r, ri, config.RoleViewer, req.Cluster, "migration.validate") {
		return
	}
	var err error
	if req.Type == RunExport {
		_, _, err = s.prepareExport(req.Export, req.Tool)
	} else {
		var cfg *config.Config
		cfg, err = s.prepareMigration(req.Migration, req.Cluster, ri.user)
		if err == nil {
			cfg.Default()
			err = cfg.Validate()
		}
	}
	if err != nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"valid": true})
}

// exportTemplate is a starting Export for a tool.
func (s *Server) exportTemplate(w http.ResponseWriter, r *http.Request, _ *reqInfo) {
	t, err := s.tool(r.URL.Query().Get("tool"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	x := &config.Export{Spec: config.ExportSpec{Provider: config.ProviderRef{Name: t.Provider}}}
	x.Metadata.Name = t.Name
	x.Default()
	x.Spec.Instances = nil
	x.Spec.Output.Dir = ""
	b, err := x.Marshal()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"export": string(b)})
}

func (s *Server) migrationSchema(w http.ResponseWriter, _ *http.Request, _ *reqInfo) {
	b, err := config.Schema()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	_, _ = w.Write(b)
}

// --- runs ------------------------------------------------------------------------

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rq := runQuery{Type: q.Get("type"), Cluster: q.Get("cluster"), Status: q.Get("status"), CreatedBy: q.Get("createdBy"), Limit: limit}
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before: an RFC 3339 time, such as a run's createdAt")
			return
		}
		rq.Before, rq.BeforeID = t, q.Get("beforeID")
	}
	list := s.runs.list(rq, func(run *Run) bool { return roleForRun(ri.grants, run) != "" })
	if list == nil {
		list = []Run{}
	}
	writeJSONResponse(w, http.StatusOK, list)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	var req runRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	busy := s.runs.list(runQuery{CreatedBy: ri.user.Username, Active: true, Limit: s.cfg.Runs.MaxQueuedPerUser}, func(*Run) bool { return true })
	if len(busy) >= s.cfg.Runs.MaxQueuedPerUser {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("you have %d runs queued or working; wait for them or cancel some", len(busy)))
		return
	}
	var run *Run
	var err error
	if req.Type == RunExport {
		if !atLeast(roleOnTool(ri.grants, req.Tool), config.RoleOperator) {
			s.deny(r, "runs.create", req.Tool, "needs operator")
			writeError(w, http.StatusForbidden, "you need the operator role on tool "+req.Tool)
			return
		}
		run, err = s.startExport(r.Context(), req, ri.user)
	} else {
		if !s.can(w, r, ri, config.RoleOperator, req.Cluster, "runs.create") {
			return
		}
		run, err = s.startRun(r.Context(), req, ri.user)
	}
	where := firstNonEmpty(req.Cluster, req.Tool)
	if err != nil {
		s.event(r, "run.create", "error", where, req.Type, err.Error())
		var se serverError
		switch {
		case errors.Is(err, errShuttingDown):
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case errors.As(err, &se):
			s.log.ErrorContext(r.Context(), "starting a run", "request", ri.id, "error", err)
			writeError(w, http.StatusInternalServerError, "the run couldn't be recorded (request "+ri.id+")")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.event(r, "run.create", "ok", where, run.ID, req.Type+" "+run.Name)
	writeJSONResponse(w, http.StatusCreated, run)
}

// visibleRun returns the run if the caller has role on its cluster.
func (s *Server) visibleRun(w http.ResponseWriter, r *http.Request, ri *reqInfo, role, action string) (*Run, bool) {
	run, ok := s.runs.get(r.PathValue("id"))
	if !ok || roleForRun(ri.grants, run) == "" {
		writeError(w, http.StatusNotFound, "no such run")
		return nil, false
	}
	if !atLeast(roleForRun(ri.grants, run), role) {
		s.deny(r, action, run.Cluster+run.Tool, "needs "+role)
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need the %s role on %s", role, firstNonEmpty(run.Cluster, run.Tool)))
		return nil, false
	}
	return run, true
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.get"); ok {
		writeJSONResponse(w, http.StatusOK, run)
	}
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.delete")
	if !ok {
		return
	}
	// The person who started it may delete it; otherwise admins of its
	// cluster or tool.
	if run.CreatedBy != ri.user.Username && !atLeast(roleForRun(ri.grants, run), config.RoleAdmin) {
		s.deny(r, "runs.delete", run.Cluster+run.Tool, "needs admin")
		writeError(w, http.StatusForbidden, "only the person who started it, or an admin, can delete a run")
		return
	}
	if err := s.runs.remove(run.ID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.event(r, "run.delete", "ok", run.Cluster+run.Tool, run.ID, "")
	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleOperator, "runs.cancel")
	if !ok {
		return
	}
	if !s.runs.cancel(run.ID) {
		writeError(w, http.StatusConflict, "the run has finished")
		return
	}
	s.event(r, "run.cancel", "ok", run.Cluster+run.Tool, run.ID, "")
	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "canceling"})
}

func (s *Server) publishRun(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleOperator, "runs.publish")
	if !ok {
		return
	}
	if run.Type == RunScan || run.Status != StatusSucceeded {
		writeError(w, http.StatusConflict, "only migrations and exports that succeeded can be pushed")
		return
	}
	var req publishRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// A push isn't stopped halfway by a closed tab: it finishes (or times
	// out) and stores the output.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	if err := s.publish(ctx, run.ID, ri.user, req.Message, nil); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errPushing) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	run, _ = s.runs.get(run.ID)
	writeJSONResponse(w, http.StatusOK, run)
}

// runEvents streams a run's log and status changes (server-sent events).
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.events")
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming isn't supported")
		return
	}
	snapshot, lines, ch, unsubscribe, ok := s.runs.subscribe(r.Context(), run.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	// A browser reconnecting sends the last event it had: carry on from there.
	from := 0
	if last, err := strconv.Atoi(r.Header.Get("Last-Event-ID")); err == nil && last >= 0 {
		from = last + 1
	}
	defer unsubscribe()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
		return err == nil
	}
	sendLog := func(seq int, l *LogLine) bool {
		if seq < from {
			return true
		}
		b, _ := json.Marshal(l)
		_, err := fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", seq, b)
		flusher.Flush()
		return err == nil
	}
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	for i := range lines {
		if !sendLog(i, &lines[i]) {
			return
		}
	}
	send("run", snapshot)
	if ch == nil {
		send("end", map[string]string{})
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				if final, ok := s.runs.get(run.ID); ok {
					send("run", final)
				}
				send("end", map[string]string{})
				return
			}
			if ev.Log != nil && !sendLog(ev.Seq, ev.Log) {
				return
			}
			if ev.Run != nil && !send("run", ev.Run) {
				return
			}
		}
	}
}

func (s *Server) runLog(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.log")
	if !ok {
		return
	}
	lines := s.runs.readLog(run.ID)
	if lines == nil {
		lines = []LogLine{}
	}
	writeJSONResponse(w, http.StatusOK, lines)
}

func (s *Server) runScan(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.scan")
	if !ok {
		return
	}
	b, err := s.runs.getBlob(run.ID, "scan.json")
	if err != nil {
		writeError(w, http.StatusNotFound, "this run has no scan result (yet)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// hidden reports files the server never serves: Git internals and the
// git-ignored files that may hold Secret values.
func hidden(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == ".git" {
			return true
		}
	}
	return secretFile(rel)
}

// secretFile reports whether a path of an output may hold Secret values:
// never served. ownSecretFile ones are never archived or pushed either.
func secretFile(rel string) bool {
	return ownSecretFile(rel) || strings.Contains(path.Base(rel), ".secrets.")
}

// ownSecretFile reports whether a path is where gitopsify writes Secret
// values in clear for the person running it, git-ignored: .secrets/ and
// *.secrets.auto.tfvars.
func ownSecretFile(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == ".secrets" {
			return true
		}
	}
	return strings.HasSuffix(rel, ".secrets.auto.tfvars")
}

// trackedSecretFiles lists the files of gitopsify's plaintext kind that the
// repository in dir tracks: git-ignored or not, a push would publish them.
func trackedSecretFiles(dir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil, nil
	}
	tracked := map[string]bool{}
	if err := trackedFiles(dir, "", tracked); err != nil {
		return nil, err
	}
	var out []string
	for f := range tracked {
		if ownSecretFile(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

func refuseTrackedSecrets(dir, remote string) error {
	files, err := trackedSecretFiles(dir)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return fmt.Errorf("%s tracks %s, where gitopsify writes Secret values in clear: remove them from the repository (git rm --cached) and keep them ignored", remote, strings.Join(files, ", "))
	}
	return nil
}

type fileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (s *Server) runFiles(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.files")
	if !ok {
		return
	}
	out := []fileEntry{}
	root, err := s.runs.ensureOutput(r.Context(), run.ID, run.OutputVersion)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, out)
		return
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && hidden(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if hidden(rel) || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			out = append(out, fileEntry{Path: rel, Size: fi.Size()})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSONResponse(w, http.StatusOK, out)
}

func (s *Server) runFile(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.file")
	if !ok {
		return
	}
	rel := path.Clean("/" + r.PathValue("path"))[1:]
	if rel == "" || hidden(rel) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	dir, err := s.runs.ensureOutput(r.Context(), run.ID, run.OutputVersion)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if fi.Size() > 8<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "the file is larger than 8 MiB; download the archive")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

func (s *Server) runArchive(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	run, ok := s.visibleRun(w, r, ri, config.RoleViewer, "runs.archive")
	if !ok {
		return
	}
	root, err := s.runs.ensureOutput(r.Context(), run.ID, run.OutputVersion)
	if err != nil {
		writeError(w, http.StatusNotFound, "this run wrote no files")
		return
	}
	name := firstNonEmpty(run.Name, "gitopsify") + "-" + run.ID
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, name))
	s.event(r, "run.download", "ok", run.Cluster+run.Tool, run.ID, "")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if hidden(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
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
		hdr.Name = name + "/" + rel
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
		s.log.Warn("writing archive", "run", run.ID, "error", err)
	}
	_ = tw.Close()
	_ = gz.Close()
}

// --- audit -------------------------------------------------------------------------

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if !atLeast(serverRole(ri.grants), config.RoleAdmin) {
		s.deny(r, "audit.list", "", "needs admin on every cluster")
		writeError(w, http.StatusForbidden, "the audit log needs the admin role without cluster limits")
		return
	}
	q := r.URL.Query()
	aq := auditQuery{Actor: q.Get("actor"), Action: q.Get("action"), Cluster: q.Get("cluster"), Outcome: q.Get("outcome")}
	aq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before: want an RFC 3339 time")
			return
		}
		aq.Before = t
	}
	evs, err := s.audit.query(aq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []AuditEvent{}
	}
	writeJSONResponse(w, http.StatusOK, evs)
}

func constantEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range len(a) {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
