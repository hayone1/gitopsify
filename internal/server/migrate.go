package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/gitrepo"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/pipeline"
)

// runRequest starts a scan or a migration.
type runRequest struct {
	Type string `json:"type"`
	// Cluster is the source cluster, by name.
	Cluster string `json:"cluster"`
	// Migration is the Migration resource (YAML or JSON text).
	Migration string `json:"migration"`
	// From is the scan this migration was configured from.
	From string `json:"from,omitempty"`
	// Publish pushes the output when the run succeeds.
	Publish *publishRequest `json:"publish,omitempty"`

	// Tool is the tool an export reads, by name.
	Tool string `json:"tool,omitempty"`
	// Export is the Export resource (YAML or JSON text).
	Export string `json:"export,omitempty"`
	// Repo is where an export's output can be pushed.
	Repo *RunRepo `json:"repo,omitempty"`
	// AllowPartial writes an export even if the provider couldn't read
	// everything.
	AllowPartial bool `json:"allowPartial,omitempty"`
}

type publishRequest struct {
	Message string `json:"message,omitempty"`
}

// appSummary is an app of a scan, as the UI lists them.
type appSummary struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace,omitempty"`
	Wave      int      `json:"wave"`
	Phase     string   `json:"phase,omitempty"`
	Objects   int      `json:"objects"`
	Kinds     []string `json:"kinds"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Helm      *helmApp `json:"helm,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

type helmApp struct {
	Release string `json:"release"`
	Chart   string `json:"chart"`
	Version string `json:"version"`
	Mode    string `json:"mode"`
}

// scanResult is what a scan found.
type scanResult struct {
	Source        string       `json:"source"`
	Apps          []appSummary `json:"apps"`
	Warnings      []string     `json:"warnings"`
	SecretActions []string     `json:"secretActions"`
	Excluded      []excluded   `json:"excluded"`
}

type excluded struct {
	Object string `json:"object"`
	Reason string `json:"reason"`
}

// prepareMigration parses a migration and makes it safe to run on the
// server: clusters only by registered name (their access comes from the
// server configuration, never from the request), no paths on the server's
// disk, no plain secret values unless the server allows them.
func (s *Server) prepareMigration(text, cluster string, u *User) (*config.Config, error) {
	cfg, err := config.ParseMigration("migration.yaml", []byte(text))
	if err != nil {
		return nil, err
	}
	if !reflect.ValueOf(cfg.Source.Access).IsZero() {
		return nil, errors.New("spec.source: pick the cluster by name; the server configures how to reach it")
	}
	c, err := s.cluster(cluster)
	if err != nil {
		return nil, err
	}
	if cfg.Source.Access, err = s.access(c, u); err != nil {
		return nil, err
	}
	for i := range cfg.Environments {
		e := &cfg.Environments[i]
		name := e.Cluster.Access.Context
		rest := e.Cluster.Access
		rest.Context = ""
		if !reflect.ValueOf(rest).IsZero() {
			return nil, fmt.Errorf("spec.environments[%d].cluster: name a registered cluster with context; the server configures how to reach it", i)
		}
		if name == "" {
			continue
		}
		ec, err := s.cluster(name)
		if err != nil {
			return nil, fmt.Errorf("spec.environments[%d].cluster.context: %w", i, err)
		}
		if role := roleOn(s.grantsOf(u), name); !atLeast(role, config.RoleViewer) {
			return nil, fmt.Errorf("spec.environments[%d]: you have no access to cluster %s", i, name)
		}
		if e.Cluster.Access, err = s.access(ec, u); err != nil {
			return nil, err
		}
	}
	if cfg.Output.Repo.Path != "" {
		return nil, errors.New("spec.output.repo.path isn't supported on the server; it writes the repository root")
	}
	if cfg.Secrets.SealedSecrets.CertFile != "" {
		return nil, errors.New("spec.secrets.sealedSecrets.certFile isn't supported on the server; the certificate is fetched from the first environment's cluster")
	}
	if !s.cfg.Runs.AllowSecretValues {
		strategies := []string{cfg.Secrets.Strategy}
		for _, o := range cfg.Secrets.Overrides {
			strategies = append(strategies, o.Strategy)
		}
		for _, st := range strategies {
			if st == config.SecretsLocal || st == config.SecretsPlain {
				return nil, fmt.Errorf("secrets strategy %q would store Secret values on the server; use sealed-secrets, sops, external-secrets or skip (an admin can set spec.runs.allowSecretValues)", st)
			}
		}
	}
	cfg.Output.Dir = "" // set per run
	return cfg, nil
}

func (s *Server) grantsOf(u *User) []grant {
	if u == nil {
		return nil
	}
	if u.Via == "token" {
		for _, t := range s.cfg.Auth.Tokens {
			if "token:"+t.Name == u.Username {
				return s.policy.grants(u, []config.ServerToken{t})
			}
		}
		return nil
	}
	return s.policy.grants(u, nil)
}

// startRun starts a scan or a migration for u.
func (s *Server) startRun(req runRequest, u *User) (*Run, error) {
	cfg, err := s.prepareMigration(req.Migration, req.Cluster, u)
	if err != nil {
		return nil, err
	}
	cfg.Default()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// The resource as submitted (with the defaults), without the access
	// settings the server filled in.
	spec, _ := json.Marshal(publicDocument(cfg, req.Cluster))
	r := Run{Type: req.Type, Name: cfg.Metadata.Name, Cluster: req.Cluster, CreatedBy: u.Username, Spec: spec, From: req.From}
	switch req.Type {
	case RunScan:
		return s.runs.start(r, func(ctx context.Context, rc *runContext) error { return s.scan(ctx, rc, cfg) })
	case RunMigrate:
		return s.runs.start(r, func(ctx context.Context, rc *runContext) error {
			if err := s.migrate(ctx, rc, cfg); err != nil {
				return err
			}
			if req.Publish != nil {
				return s.publish(rc.ID(), u, req.Publish.Message, rc)
			}
			return nil
		})
	}
	return nil, fmt.Errorf("unknown run type %q: want scan or migrate", req.Type)
}

// publicDocument is the migration without server-side access settings,
// with environment clusters named by their registered name.
func publicDocument(cfg *config.Config, cluster string) any {
	c := *cfg
	c.Source.Access = config.Access{}
	c.Environments = append([]config.Environment(nil), cfg.Environments...)
	for i := range c.Environments {
		c.Environments[i].Cluster.Access = config.Access{Context: cfg.Environments[i].Cluster.Access.Context}
	}
	c.Output.Dir = ""
	_ = cluster
	return c.Document()
}

func (s *Server) pipelineOptions(rc *runContext, cfg *config.Config) pipeline.Options {
	return pipeline.Options{Config: cfg, Version: s.version, Connect: kube.ConnectOptions{NoInput: true},
		Log: func(format string, args ...any) { rc.Info(format, args...) }}
}

func (s *Server) scan(ctx context.Context, rc *runContext, cfg *config.Config) error {
	rc.Info("scanning %s", firstNonEmpty(cfg.Source.Access.Context, "the cluster"))
	res, err := pipeline.Scan(ctx, s.pipelineOptions(rc, cfg))
	if err != nil {
		return err
	}
	out := summarize(res)
	if err := writeJSON(filepath.Join(s.runs.runDir(rc.ID()), "scan.json"), out); err != nil {
		return err
	}
	rc.update(func(r *Run) { r.Summary = summaryOf(res, 0) })
	rc.Info("found %d apps with %d objects", len(out.Apps), r0(res))
	return nil
}

func r0(res *pipeline.Result) int {
	n := 0
	for _, a := range res.Inventory.Apps {
		n += len(a.Objects)
	}
	return n
}

func (s *Server) migrate(ctx context.Context, rc *runContext, cfg *config.Config) error {
	dir := s.runs.outputDir(rc.ID())
	cfg.Output.Dir = dir
	// Continue the remote's history, as generate --push does.
	if cfg.Output.Repo.URL != "" {
		for d, remote := range layoutDirs(cfg) {
			env, err := gitEnv(s.cfg.Git.Credentials, remote)
			if err != nil {
				return err
			}
			done, err := gitrepo.CheckoutEnv(d, remote, cfg.Output.Repo.Revision, env)
			if err != nil {
				rc.Warn("couldn't check out %s (%v); the output starts a new history and pushing it may be refused", remote, err)
				continue
			}
			if done {
				rc.Info("checked out %s (%s)", remote, firstNonEmpty(cfg.Output.Repo.Revision, "main"))
			}
		}
	}
	popts := s.pipelineOptions(rc, cfg)
	rc.Info("scanning %s", firstNonEmpty(cfg.Source.Access.Context, "the cluster"))
	res, err := pipeline.Scan(ctx, popts)
	if err != nil {
		return err
	}
	if err := pipeline.Finish(ctx, popts, res); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.runs.runDir(rc.ID()), "scan.json"), summarize(res)); err != nil {
		return err
	}
	written, _, err := res.Tree.Write(dir, true)
	if err != nil {
		return err
	}
	rc.Info("wrote %d files for %d apps", len(written), len(res.Inventory.Apps))
	sum := summaryOf(res, len(written))
	if sum.Warnings > 0 {
		rc.Warn("%d warnings: see MIGRATION_REPORT.md", sum.Warnings)
	}
	rc.update(func(r *Run) { r.Summary = sum })
	return nil
}

// publish commits and pushes a migration's or an export's output.
func (s *Server) publish(id string, u *User, message string, rc *runContext) error {
	r, ok := s.runs.get(id)
	if !ok {
		return os.ErrNotExist
	}
	var cfg *config.Config
	if r.Type == RunExport {
		if r.Repo == nil || r.Repo.URL == "" {
			return errors.New("the export has no repository to push to")
		}
		cfg = &config.Config{Output: config.Output{Repo: config.Repo{URL: r.Repo.URL, Revision: r.Repo.Branch}}}
	} else {
		var err error
		if cfg, err = config.ParseMigration("migration.json", r.Spec); err != nil {
			return err
		}
	}
	cfg.Output.Dir = s.runs.outputDir(id)
	if cfg.Output.Repo.URL == "" && (cfg.Output.Layout != config.LayoutRepoPerEnv || len(cfg.Environments) == 0 || cfg.Environments[0].RepoURL == "") {
		return errors.New("the migration has no spec.output.repo.url to push to")
	}
	if message == "" {
		message = fmt.Sprintf("gitopsify: %s from %s (run %s)", firstNonEmpty(r.Name, "migration"), firstNonEmpty(r.Cluster, r.Tool), id)
	}
	name, email := s.cfg.Git.AuthorName, s.cfg.Git.AuthorEmail
	if name == "" {
		name = firstNonEmpty(u.Name, "gitopsify")
	}
	if email == "" {
		email = firstNonEmpty(u.Email, "gitopsify@users.noreply.github.com")
	}
	dirs := layoutDirs(cfg)
	keys := make([]string, 0, len(dirs))
	for d := range dirs {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	var failed error
	for _, d := range keys {
		remote := dirs[d]
		p := Publish{At: time.Now().UTC(), By: u.Username, Dir: strings.TrimPrefix(strings.TrimPrefix(d, cfg.Output.Dir), "/"),
			Remote: remote, Branch: firstNonEmpty(cfg.Output.Repo.Revision, "main")}
		env, err := gitEnv(s.cfg.Git.Credentials, remote)
		if err == nil {
			var res *gitrepo.Result
			res, err = gitrepo.Publish(gitrepo.Options{Dir: d, Remote: remote, Branch: p.Branch, Message: message, Push: true,
				AuthorName: name, AuthorEmail: email, Env: env})
			if err == nil {
				p.Commit, p.Pushed = res.Commit, res.Pushed
			}
		}
		if err != nil {
			p.Error = err.Error()
			failed = err
		}
		if rc != nil {
			if err != nil {
				rc.Warn("pushing to %s failed: %v", remote, err)
			} else {
				rc.Info("pushed %s to %s (%s)", p.Commit, remote, p.Branch)
			}
		}
		if st, ok := s.runs.state(id); ok {
			s.runs.update(st, func(r *Run) { r.Publishes = append(r.Publishes, p) })
		}
	}
	return failed
}

// layoutDirs returns the repository directories of the output and their
// remotes: one, or one per environment with repo-per-env.
func layoutDirs(cfg *config.Config) map[string]string {
	if cfg.Output.Layout == config.LayoutRepoPerEnv {
		out := map[string]string{}
		for _, e := range cfg.Environments {
			out[filepath.Join(cfg.Output.Dir, e.Name)] = firstNonEmpty(e.RepoURL, cfg.Output.Repo.URL)
		}
		return out
	}
	return map[string]string{cfg.Output.Dir: cfg.Output.Repo.URL}
}

func summarize(res *pipeline.Result) scanResult {
	inv := res.Inventory
	out := scanResult{Source: inv.Source.String(), Warnings: append([]string{}, inv.Warnings...), SecretActions: append([]string{}, inv.SecretActions...)}
	for _, a := range inv.Apps {
		kinds := map[string]bool{}
		for _, o := range a.Objects {
			kinds[o.Ref.Kind] = true
		}
		ks := make([]string, 0, len(kinds))
		for k := range kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		as := appSummary{Name: a.Name, Kind: string(a.Kind), Namespace: a.Namespace, Wave: a.Wave, Phase: string(a.Phase),
			Objects: len(a.Objects), Kinds: ks, DependsOn: a.DependsOn, Notes: a.Notes}
		if a.Release != nil {
			as.Helm = &helmApp{Release: a.Release.Namespace + "/" + a.Release.Name, Chart: a.Release.Chart.Name,
				Version: a.Release.Chart.Version, Mode: string(a.Release.Mode)}
		}
		out.Apps = append(out.Apps, as)
	}
	for _, o := range inv.Objects {
		if o.Excluded {
			out.Excluded = append(out.Excluded, excluded{Object: o.Ref.String(), Reason: o.Reason})
		}
	}
	if out.Apps == nil {
		out.Apps = []appSummary{}
	}
	return out
}

func summaryOf(res *pipeline.Result, files int) *RunSummary {
	inv := res.Inventory
	s := &RunSummary{Apps: len(inv.Apps), Files: files, Warnings: len(inv.Warnings), SecretActions: len(inv.SecretActions)}
	for _, o := range inv.Objects {
		if o.Excluded {
			s.Excluded++
		}
	}
	s.Objects = r0(res)
	return s
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
