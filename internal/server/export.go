package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/iac"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/provider"
	"github.com/hayone1/gitopsify/sdk"
)

// roleForRun is the caller's role on what a run read: its cluster, or its
// tool for exports.
func roleForRun(gs []grant, r *Run) string {
	if r.Tool != "" {
		return roleOnTool(gs, r.Tool)
	}
	return roleOn(gs, r.Cluster)
}

// toolInfo is a tool as the API lists it.
type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Provider    string `json:"provider"`
	Role        string `json:"role"`
	// Installed reports whether the server has the provider.
	Installed bool   `json:"installed"`
	Title     string `json:"title,omitempty"`
}

func (s *Server) tool(name string) (*config.ServerTool, error) {
	for i := range s.cfg.Tools {
		if s.cfg.Tools[i].Name == name {
			return &s.cfg.Tools[i], nil
		}
	}
	return nil, fmt.Errorf("no tool named %q", name)
}

// describes caches what installed providers say about themselves.
type describes struct {
	mu   sync.Mutex
	byID map[string]*sdk.Description // path@modtime
}

func (s *Server) describe(ctx context.Context, name string) (*provider.Installed, *sdk.Description, error) {
	in, err := provider.Find(name)
	if err != nil {
		return nil, nil, fmt.Errorf("the server has no provider %s: an admin installs it (Tools page) or the image brings it", name)
	}
	key := in.Path
	if fi, err := os.Stat(in.Path); err == nil {
		key += "@" + fi.ModTime().String()
	}
	s.descs.mu.Lock()
	d, ok := s.descs.byID[key]
	s.descs.mu.Unlock()
	if ok {
		return in, d, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err = (&provider.Client{Name: in.Name, Path: in.Path, Env: s.providerEnv()}).Describe(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.descs.mu.Lock()
	s.descs.byID[key] = d
	s.descs.mu.Unlock()
	return in, d, nil
}

// toolSettings resolves a tool's connection settings: configured ones, then
// the provider's environment variables, as gitopsify export does.
func toolSettings(t *config.ServerTool, d *sdk.Description, deny []string) (map[string]string, error) {
	out := map[string]string{}
	for name, v := range t.Settings {
		val, err := v.Resolve()
		if err != nil {
			return nil, fmt.Errorf("tool %s, setting %s: %w", t.Name, name, err)
		}
		out[name] = val
	}
	var missing []string
	for _, st := range d.Settings {
		if _, ok := out[st.Name]; ok {
			continue
		}
		// From the provider's variable, unless another tool's or the
		// server's secrets come from it (a provider could name any).
		if st.Env != "" && !contains(deny, st.Env) && !strings.HasPrefix(st.Env, "GITOPSIFY_") {
			if v := os.Getenv(st.Env); v != "" {
				out[st.Name] = v
				continue
			}
		}
		if st.Default != "" {
			out[st.Name] = st.Default
			continue
		}
		if st.Required {
			missing = append(missing, st.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("tool %s lacks settings %s (spec.tools[].settings)", t.Name, strings.Join(missing, ", "))
	}
	return out, nil
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	out := []toolInfo{}
	for _, t := range s.cfg.Tools {
		role := roleOnTool(ri.grants, t.Name)
		if role == "" {
			continue
		}
		ti := toolInfo{Name: t.Name, Description: t.Description, Provider: t.Provider, Role: role}
		if _, d, err := s.describe(r.Context(), t.Provider); err == nil {
			ti.Installed, ti.Title = true, d.Title
		}
		out = append(out, ti)
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// toolCheck is what the provider sees of a tool, for the export wizard.
type toolCheck struct {
	Description *sdk.Description `json:"description"`
	Check       *sdk.CheckResult `json:"check,omitempty"`
	Error       string           `json:"error,omitempty"`
}

func (s *Server) checkTool(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	name := r.PathValue("name")
	if !atLeast(roleOnTool(ri.grants, name), config.RoleViewer) {
		s.deny(r, "tools.check", name, "needs viewer")
		writeError(w, http.StatusForbidden, "you have no role on tool "+name)
		return
	}
	t, err := s.tool(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	in, d, err := s.describe(r.Context(), t.Provider)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, toolCheck{Error: err.Error()})
		return
	}
	res := toolCheck{Description: d}
	settings, err := toolSettings(t, d, s.secretEnvsBut(t))
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		res.Check, err = (&provider.Client{Name: in.Name, Path: in.Path, Env: s.providerEnv()}).Check(ctx, sdk.Request{Instance: t.Name, Settings: settings})
	}
	if err != nil {
		res.Error = err.Error()
	}
	writeJSONResponse(w, http.StatusOK, res)
}

// providerInfo is a provider in the catalog: installed, listed in the index,
// or both.
type providerInfo struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Maturity    string   `json:"maturity,omitempty"`
	Outputs     []string `json:"outputs,omitempty"`
	Installed   string   `json:"installed,omitempty"` // version
	Kinds       []string `json:"kinds,omitempty"`
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request, _ *reqInfo) {
	byName := map[string]*providerInfo{}
	if list, err := provider.List(); err == nil {
		for _, in := range list {
			p := &providerInfo{Name: in.Name, Source: in.Source, Installed: firstNonEmpty(in.Version, "local")}
			if _, d, err := s.describe(r.Context(), in.Name); err == nil {
				p.Title, p.Description = d.Title, d.Description
				for _, k := range d.Kinds {
					p.Kinds = append(p.Kinds, k.Name)
				}
			}
			byName[in.Name] = p
		}
	}
	if idx, err := provider.LoadIndex(r.Context(), s.cfg.Providers.Index); err == nil {
		for _, e := range idx.Providers {
			p := byName[e.Name]
			if p == nil {
				p = &providerInfo{Name: e.Name}
				byName[e.Name] = p
			}
			p.Source = firstNonEmpty(p.Source, e.Source)
			p.Description = firstNonEmpty(e.Description, p.Description)
			p.Homepage, p.Maturity, p.Outputs = e.Homepage, e.Maturity, e.Outputs
		}
	}
	out := make([]providerInfo, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSONResponse(w, http.StatusOK, out)
}

type installRequest struct {
	// Source is a name from the index or owner/name.
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
}

func (s *Server) installProvider(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if !atLeast(serverRole(ri.grants), config.RoleAdmin) {
		s.deny(r, "providers.install", "", "needs admin everywhere")
		writeError(w, http.StatusForbidden, "installing providers needs the admin role without cluster or tool limits")
		return
	}
	if s.cfg.Providers.DisableInstall {
		writeError(w, http.StatusForbidden, "this server doesn't install providers (spec.providers.disableInstall)")
		return
	}
	var req installRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	src, _, err := provider.Resolve(ctx, req.Source, s.cfg.Providers.Index)
	if err == nil {
		var in *provider.Installed
		in, err = (&provider.Installer{Dir: s.cfg.Providers.Dir}).Install(ctx, src, req.Version)
		if err == nil {
			s.event(r, "provider.install", "ok", "", in.Name, in.Source+" "+in.Version)
			writeJSONResponse(w, http.StatusOK, providerInfo{Name: in.Name, Source: in.Source, Installed: in.Version})
			return
		}
	}
	s.event(r, "provider.install", "error", "", req.Source, err.Error())
	writeError(w, http.StatusBadGateway, err.Error())
}

// prepareExport parses an export and makes it safe to run on the server:
// the tool's provider and connection come from the server configuration.
func (s *Server) prepareExport(text, toolName string) (*config.Export, *config.ServerTool, error) {
	t, err := s.tool(toolName)
	if err != nil {
		return nil, nil, err
	}
	x, err := config.ParseExport("export.yaml", []byte(text))
	if err != nil {
		return nil, nil, err
	}
	if x.Spec.Provider.Name != "" && x.Spec.Provider.Name != t.Provider {
		return nil, nil, fmt.Errorf("tool %s is read by provider %s, not %s", t.Name, t.Provider, x.Spec.Provider.Name)
	}
	if len(x.Spec.Instances) > 0 {
		return nil, nil, errors.New("spec.instances: pick the tool by name; the server configures how to reach it")
	}
	if x.Spec.Output.SecretValues == config.SecretValuesLocal && !s.cfg.Runs.AllowSecretValues {
		return nil, nil, errors.New("output.secretValues: local would store secret values on the server (an admin can set spec.runs.allowSecretValues)")
	}
	x.Spec.Provider = config.ProviderRef{Name: t.Provider}
	x.Spec.Instances = []config.ExportInstance{{Name: t.Name}}
	x.Spec.Output.Dir = ""
	x.Default()
	if err := x.Validate(); err != nil {
		return nil, nil, err
	}
	return x, t, nil
}

// exportDocument is the export as the repository keeps it for CI: the
// tool's settings as the provider's environment variables, never values.
func exportDocument(x *config.Export, d *sdk.Description, t *config.ServerTool) *config.Export {
	c := *x
	c.Spec.Instances = []config.ExportInstance{{Name: t.Name, Settings: map[string]config.SettingValue{}}}
	for _, st := range d.Settings {
		if v, ok := t.Settings[st.Name]; ok && v.Literal() && !st.Secret {
			c.Spec.Instances[0].Settings[st.Name] = v
		} else if st.Env != "" {
			c.Spec.Instances[0].Settings[st.Name] = config.SettingValue{Env: st.Env}
		}
	}
	c.Spec.Output.Dir = ""
	return &c
}

func (s *Server) startExport(ctx context.Context, req runRequest, u *User) (*Run, error) {
	x, t, err := s.prepareExport(req.Export, req.Tool)
	if err != nil {
		return nil, err
	}
	if req.Repo != nil {
		if err := s.checkRepo(req.Repo.URL, req.Repo.Branch, "", t.Name); err != nil {
			return nil, fmt.Errorf("repo: %w", err)
		}
	}
	if req.Publish != nil && (req.Repo == nil || req.Repo.URL == "") {
		return nil, errors.New("publish: name the repository to push to (repo.url)")
	}
	in, d, err := s.describe(context.Background(), t.Provider)
	if err != nil {
		return nil, err
	}
	settings, err := toolSettings(t, d, s.secretEnvsBut(t))
	if err != nil {
		return nil, err
	}
	spec, _ := json.Marshal(exportDocument(x, d, t))
	r := Run{Type: RunExport, Name: firstNonEmpty(x.Metadata.Name, t.Name), Tool: t.Name, CreatedBy: u.Username, Spec: spec, Repo: req.Repo}
	return s.runs.start(ctx, r, func(ctx context.Context, rc *runContext) error {
		if err := s.export(ctx, rc, x, t, in, d, settings, req.AllowPartial, req.Repo); err != nil {
			return err
		}
		if req.Publish != nil {
			if err := s.publish(ctx, rc.ID(), u, req.Publish.Message, rc); err != nil {
				rc.Warn("not pushed: %v", err)
			}
		}
		return nil
	})
}

func (s *Server) export(ctx context.Context, rc *runContext, x *config.Export, t *config.ServerTool, in *provider.Installed,
	d *sdk.Description, settings map[string]string, allowPartial bool, repo *RunRepo) error {
	// The repository the export goes to: its branch, and the state of the
	// export already there (stable names, removed blocks), come first.
	dir := s.runs.outputDir(rc.ID())
	if repo != nil {
		if err := s.checkoutRemote(ctx, rc, dir, repo.URL, repo.Branch); err != nil {
			return err
		}
	}
	prev, err := iac.LoadState(dir)
	if err != nil {
		return fmt.Errorf("the export state in %s: %w", firstNonEmpty(repoURL(repo), "the output"), err)
	}
	client := &provider.Client{Name: in.Name, Path: in.Path, Env: s.providerEnv(), Log: func(l provider.LogLine) {
		switch l.Level {
		case "DEBUG":
		case "WARN", "ERROR":
			rc.Warn("%s", l.String())
		default:
			rc.Info("%s", l.String())
		}
	}}
	rc.Info("connecting to %s with %s %s", t.Name, in.Name, firstNonEmpty(in.Version, d.Version))
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	check, err := client.Check(cctx, sdk.Request{Instance: t.Name, Settings: settings})
	cancel()
	if err != nil {
		return err
	}
	rc.Info("%s, %d %s", firstNonEmpty(check.Target, t.Name), len(check.Partitions), d.Partition.Plural)
	sel := &x.Spec.Select
	ids, err := iac.MatchPartitions(check.Partitions, sel.Partitions)
	if err != nil {
		return err
	}
	kinds := sel.Kinds
	if len(kinds) == 0 {
		for _, k := range d.Kinds {
			if !k.Optional && ((k.Terraform != nil && d.Terraform != nil) || (k.Crossplane != nil && d.Crossplane != nil)) {
				kinds = append(kinds, k.Name)
			}
		}
	}
	n := 0
	inv, err := client.Discover(ctx, sdk.Request{Instance: t.Name, Settings: settings, Scope: sdk.Scope{Partitions: ids, Kinds: kinds}},
		func(sdk.Resource) { n++ })
	if err != nil {
		return err
	}
	for _, k := range check.Kinds {
		if !k.OK && contains(kinds, k.Kind) {
			inv.Warnings = append(inv.Warnings, sdk.Warning{Kind: k.Kind, Message: firstNonEmpty(k.Message, "can't be read")})
		}
	}
	rc.Info("read %d objects", n)
	for _, w := range inv.Warnings {
		rc.Warn("couldn't read %s %s: %s", w.Partition, w.Kind, w.Message)
	}
	if len(inv.Warnings) > 0 && !allowPartial {
		return errors.New("the provider couldn't read everything (see the log); fix the access, or allow a partial export")
	}
	var selected map[string]bool
	if len(sel.Include) > 0 || len(sel.Exclude) > 0 {
		picked := iac.Select(inv, iac.Selection{Include: sel.Include, Exclude: sel.Exclude, NoDeps: sel.NoDeps})
		selected = map[string]bool{}
		for _, r := range picked.Resources {
			selected[r.Key] = true
		}
		inv.Skipped = picked.Skipped
	}
	res, err := iac.Build(iac.Input{Description: d, Previous: prev, Instances: []*iac.Instance{{Name: t.Name, Settings: settings, Target: check.Target,
		Kinds: kinds, Inventory: inv, Selected: selected}}, Options: iac.Options{
		Formats: x.Spec.Output.Formats, Adopt: x.Spec.Output.AdoptOrDefault(), SecretValues: firstNonEmpty(x.Spec.Output.SecretValues, config.SecretValuesSkip),
		NamespacePrefix: x.Spec.Output.Crossplane.NamespacePrefix, ManagementPolicies: x.Spec.Output.Crossplane.ManagementPolicies,
		Backend: x.Spec.Output.Terraform.Backend, Prune: sel.Prune,
	}})
	if err != nil {
		return err
	}
	written, _, err := res.Write(dir, true)
	if err != nil {
		return err
	}
	// The export resource, so CI can run the same export.
	doc, err := exportDocument(x, d, t).Marshal()
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, config.DefaultExportFile(d.Name)), append([]byte(config.ExportSchemaHeader), doc...), 0o644)
	}
	if err != nil {
		return err
	}
	rep := res.Report
	rc.Info("wrote %d files: %d objects in %d workspaces", len(written)+1, rep.Total(), len(rep.Workspaces))
	for _, m := range rep.MissingSecrets {
		rc.Warn("secret not in the output: %s", m)
	}
	rc.update(func(r *Run) {
		r.Summary = &RunSummary{Apps: len(rep.Workspaces), Objects: rep.Total(), Excluded: len(rep.Skipped), Files: len(written) + 1,
			Warnings: len(inv.Warnings), SecretActions: len(rep.MissingSecrets) + len(rep.Variables) + len(rep.Secrets)}
	})
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// providerEnv is what providers see of the server's environment: what
// programs need to run and reach the network, never the server's own
// secrets (session key, OIDC client secret, database URL, Git tokens).
// Tool settings reach them in the request.
func (s *Server) providerEnv() []string {
	if s.local {
		return nil // gitopsify ui: like the CLI
	}
	return providerEnv(os.Environ(), s.cfg.Providers.Env, settingEnvs(reflect.ValueOf(s.cfg)))
}

func providerEnv(environ, allow, deny []string) []string {
	out := []string{} // never nil: nil would pass everything
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if contains(deny, name) || strings.HasPrefix(name, "GITOPSIFY_") {
			continue
		}
		switch strings.ToUpper(name) {
		case "PATH", "HOME", "TMPDIR", "TZ", "LANG", "USER", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
			"SSL_CERT_FILE", "SSL_CERT_DIR", "GODEBUG":
			out = append(out, kv)
		default:
			if strings.HasPrefix(name, "LC_") || match.Glob(allow, name) {
				out = append(out, kv)
			}
		}
	}
	return out
}

// secretEnvsBut are the variables the configuration's secrets come from,
// but those of tool t's own settings: t's provider may read no other
// (another tool's credentials, the server's).
func (s *Server) secretEnvsBut(t *config.ServerTool) []string {
	own := settingEnvs(reflect.ValueOf(t))
	var out []string
	for _, e := range settingEnvs(reflect.ValueOf(s.cfg)) {
		if !contains(own, e) {
			out = append(out, e)
		}
	}
	return out
}

// settingEnvs lists the environment variables the configuration reads
// settings from (its secrets): providers never get them.
func settingEnvs(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return settingEnvs(v.Elem())
	case reflect.Struct:
		if sv, ok := v.Interface().(config.SettingValue); ok {
			if sv.Env != "" {
				return []string{sv.Env}
			}
			return nil
		}
		var out []string
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out = append(out, settingEnvs(v.Field(i))...)
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		var out []string
		for i := range v.Len() {
			out = append(out, settingEnvs(v.Index(i))...)
		}
		return out
	case reflect.Map:
		var out []string
		for it := v.MapRange(); it.Next(); {
			out = append(out, settingEnvs(it.Value())...)
		}
		return out
	}
	return nil
}

func repoURL(r *RunRepo) string {
	if r == nil {
		return ""
	}
	return r.URL
}
