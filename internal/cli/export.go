package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/iac"
	"github.com/hayone1/gitopsify/pkg/provider"
	"github.com/hayone1/gitopsify/sdk"
)

type exportFlags struct {
	sets         []string
	partitions   []string
	kinds        []string
	include      []string
	exclude      []string
	noDeps       bool
	prune        bool
	to           []string
	noAdopt      bool
	secretValues string
	dryRun       bool
	save         bool
	allowPartial bool
	install      bool
	index        string
	timeout      time.Duration
}

func newExport(o *options) *cobra.Command {
	f := &exportFlags{}
	cmd := &cobra.Command{
		Use:   "export [provider]",
		Short: "Turn a tool's live state (Grafana, ...) into Terraform or Crossplane",
		Long: `Reads what a tool holds through a provider (see "gitopsify provider") and writes
it as Terraform and/or Crossplane configuration: one Terraform root module and
one Kustomization per instance and partition (Grafana organization, Keycloak
realm, ...), with stable names, files for big JSON documents, and secrets kept
out of Git.

By default the configuration adopts the objects that were read: Terraform gets
import blocks and Crossplane objects get external names, so the first plan
changes nothing and deleting the configuration never deletes the objects.
--no-adopt writes configuration that creates them in another instance instead.

On a terminal, export asks for what flags and the export file don't say, and
offers to save the answers to gitopsify-<provider>.yaml. In CI, run it with
--no-input: it reads that file, takes credentials from the environment, and
fails if the provider couldn't read everything (unless --allow-partial).

Exporting again into the same directory updates what changed in the tool. Names
stay stable across renames, and objects that are no longer exported get
Terraform "removed" blocks that forget them without destroying them.`,
		Example: `  gitopsify export                                   # wizard
  gitopsify export grafana --to terraform --partition main-org
  GRAFANA_URL=https://grafana.example.com GRAFANA_AUTH=admin:secret \
    gitopsify export grafana --no-input --kind Folder,Dashboard
  gitopsify export grafana --include 'Dashboard/main-org/kubernetes-*' --dry-run
  gitopsify export --no-input                        # replays gitopsify-<provider>.yaml`,
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{noClusterFlags: ""},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd, o, f, args)
		},
	}
	fs := cmd.Flags()
	fs.StringArrayVar(&f.sets, "set", nil, "connection setting: name=value, or instance.name=value with several instances (repeatable; see gitopsify provider info)")
	fs.StringSliceVar(&f.partitions, "partition", nil, "export only these partitions (organizations, realms...): names, IDs or globs (repeatable)")
	fs.StringSliceVar(&f.kinds, "kind", nil, "export only these kinds (default: the provider's default kinds)")
	fs.StringArrayVar(&f.include, "include", nil, "export only objects matching Kind/partition/name (globs; plus what they reference) (repeatable)")
	fs.StringArrayVar(&f.exclude, "exclude", nil, "leave out objects matching Kind/partition/name (globs) (repeatable)")
	fs.BoolVar(&f.noDeps, "no-deps", false, "with --include, don't pull in referenced objects")
	fs.BoolVar(&f.prune, "prune", false, "stop managing objects exported before that the selection now leaves out (Terraform forgets them; their Crossplane files are removed)")
	fs.StringSliceVar(&f.to, "to", nil, "formats to write: terraform, crossplane (default: all the provider supports)")
	fs.BoolVar(&f.noAdopt, "no-adopt", false, "write configuration that creates the objects (no import blocks or external names)")
	fs.StringVar(&f.secretValues, "secret-values", "", "secret values the tool returns: skip (variables/Secrets you fill in) or local (written to git-ignored files)")
	fs.StringVarP(&o.output, "output", "o", "", "output directory (default ./<provider>)")
	fs.BoolVar(&o.force, "force", false, "write into a non-empty directory that holds no earlier export")
	fs.BoolVar(&f.dryRun, "dry-run", false, "read and show what would be written, without writing")
	fs.BoolVar(&f.save, "save", false, "save the choices to the export file (gitopsify-<provider>.yaml)")
	fs.BoolVar(&f.allowPartial, "allow-partial", false, "write even if the provider couldn't read everything, keeping the previous files of what it couldn't read")
	fs.BoolVar(&f.install, "install", false, "install the provider (the version the export file pins) if it's missing")
	fs.StringVar(&f.index, "index", "", "provider index: a URL or a file")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Minute, "give up reading after this long")
	complete(fs, "to", config.FormatTerraform, config.FormatCrossplane)
	complete(fs, "secret-values", config.SecretValuesSkip, config.SecretValuesLocal)
	return cmd
}

// exportRun carries one export's state between steps.
type exportRun struct {
	u        *ui.UI
	o        *options
	f        *exportFlags
	cfg      *config.Export
	cfgPath  string
	fromFile bool
	loadedAs config.ExportID // the export's identity in cfgPath, as read
	p        *provider.Installed
	client   *provider.Client
	d        *sdk.Description
	insts    []*runInstance
	// asked records that the wizard decided something, so saving is offered.
	asked bool
	// typedSecret is set when a secret was typed rather than read from the
	// environment.
	typedSecret map[string]bool
	// fromEnv records settings read from the environment, by
	// "<instance>/<setting>", so saving keeps them there.
	fromEnv map[string]string
	// outputFlag is set when -o was given.
	outputFlag bool
}

type runInstance struct {
	cfg      *config.ExportInstance
	settings map[string]string
	check    *sdk.CheckResult
	ids      []string
	// keys are the objects picked in the wizard (and what they need).
	keys []string
	inv  *sdk.Inventory
	// selected holds the keys the selection keeps; nil keeps everything.
	selected map[string]bool
}

func runExport(cmd *cobra.Command, o *options, f *exportFlags, args []string) error {
	ctx := cmd.Context()
	r := &exportRun{u: o.ui(), o: o, f: f, typedSecret: map[string]bool{}, fromEnv: map[string]string{}}
	ref := ""
	if len(args) == 1 {
		ref = args[0]
	}
	if err := r.loadConfig(ref); err != nil {
		return err
	}
	if ref == "" {
		ref = firstNonEmpty(r.cfg.Spec.Provider.Source, r.cfg.Spec.Provider.Name)
	}
	if ref == "" {
		if !r.u.Interactive {
			return fmt.Errorf("which provider? Run gitopsify export <provider> (gitopsify provider list shows them)")
		}
		var err error
		if ref, err = r.pickProvider(ctx); err != nil {
			return err
		}
		r.asked = true
	}
	if err := r.ensureProvider(ctx, ref); err != nil {
		return err
	}
	if err := r.applyFlags(cmd); err != nil {
		return err
	}
	if err := r.connect(ctx); err != nil {
		return err
	}
	if err := r.choose(ctx); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	if err := r.discover(dctx); err != nil {
		return err
	}
	return r.write()
}

// loadConfig reads the export file: --config, gitopsify-<provider>.yaml, or
// the only gitopsify-*.yaml export file in the directory.
func (r *exportRun) loadConfig(ref string) error {
	path := r.o.configFile
	if path == "" && ref != "" {
		if def := config.DefaultExportFile(provider.NameOf(ref)); fileExists(def) {
			path = def
		}
	}
	if path == "" && ref == "" {
		matches, _ := filepath.Glob("gitopsify-*.yaml")
		var exports []string
		for _, m := range matches {
			if _, err := config.LoadExport(m, ""); err == nil {
				exports = append(exports, m)
			}
		}
		switch len(exports) {
		case 1:
			path = exports[0]
		case 0:
		default:
			return fmt.Errorf("several export files here (%s); name the provider or use --config", strings.Join(exports, ", "))
		}
	}
	r.cfg = &config.Export{}
	if path != "" {
		name := ""
		if ref != "" {
			name = provider.NameOf(ref)
		}
		cfg, err := config.LoadExport(path, name)
		if err != nil {
			return err
		}
		r.cfg, r.cfgPath, r.fromFile, r.loadedAs = cfg, path, true, config.IDOf(cfg)
		if ref != "" && cfg.Spec.Provider.Name != "" && cfg.Spec.Provider.Name != provider.NameOf(ref) {
			return fmt.Errorf("%s is for provider %s, not %s", path, cfg.Spec.Provider.Name, provider.NameOf(ref))
		}
		r.u.Step("using %s", path)
	}
	return nil
}

func (r *exportRun) pickProvider(ctx context.Context) (string, error) {
	list, _ := provider.List()
	var opts []ui.Option
	for _, p := range list {
		label := p.Name
		if p.Version != "" {
			label += " " + p.Version
		}
		opts = append(opts, ui.Option{Label: label, Value: p.Name})
	}
	const another = "\x00install"
	opts = append(opts, ui.Option{Label: "Install a provider from the index…", Value: another})
	choice := ""
	if len(list) > 0 {
		choice = list[0].Name
	}
	if err := r.u.Select("Which tool do you want to export?", "Providers read a tool's state; gitopsify writes it as Terraform or Crossplane.", opts, &choice); err != nil {
		return "", err
	}
	if choice != another {
		return choice, nil
	}
	idx, err := provider.LoadIndex(ctx, r.f.index)
	if err != nil {
		return "", err
	}
	opts = nil
	for _, e := range idx.Search("") {
		opts = append(opts, ui.Option{Label: e.Name + ": " + e.Description, Value: e.Name})
	}
	if len(opts) == 0 {
		return "", fmt.Errorf("the provider index (%s) is empty", idx.Location)
	}
	choice = opts[0].Value
	if err := r.u.Select("Provider to install", "From "+idx.Location, opts, &choice); err != nil {
		return "", err
	}
	if _, err := installProvider(ctx, r.u, choice, r.f.index, nil); err != nil {
		return "", err
	}
	return choice, nil
}

// ensureProvider finds the provider (installing it if allowed), describes it
// and checks the pinned version.
func (r *exportRun) ensureProvider(ctx context.Context, ref string) error {
	pin := r.cfg.Spec.Provider.Version
	installRef := ref
	if pin != "" && !strings.Contains(ref, "@") && !provider.IsPath(ref) {
		installRef = ref + "@" + pin
	}
	p, err := provider.Find(ref)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		ok := r.f.install
		if !ok && r.u.Interactive {
			ok, err = r.u.Confirm(fmt.Sprintf("Provider %s isn't installed. Install it?", provider.NameOf(ref)), "From its GitHub releases, checked against their checksums.", true)
			if err != nil {
				return err
			}
		}
		if !ok {
			return notInstalled(installRef, err)
		}
		if p, err = installProvider(ctx, r.u, installRef, r.f.index, r.cfg.Spec.Provider.Checksums); err != nil {
			return err
		}
	case err != nil:
		return err
	case pin != "" && p.Origin == provider.OriginInstalled && !sameVersion(p.Version, pin):
		ok := r.f.install
		if !ok && r.u.Interactive {
			ok, err = r.u.Confirm(fmt.Sprintf("%s pins %s %s, but %s is installed. Install %s?", r.cfgPath, p.Name, pin, p.Version, pin), "", true)
			if err != nil {
				return err
			}
		}
		if !ok {
			return &exitError{code: 1, msg: fmt.Sprintf("%s pins %s %s, but %s is installed. Install it with:\n  gitopsify provider install %s", r.cfgPath, p.Name, pin, p.Version, installRef)}
		}
		if p, err = installProvider(ctx, r.u, installRef, r.f.index, r.cfg.Spec.Provider.Checksums); err != nil {
			return err
		}
	}
	for _, s := range p.Shadowed {
		r.u.Warn("%s is not used; %s comes first", s, p.Path)
	}
	if p.Origin != provider.OriginInstalled && len(r.cfg.Spec.Provider.Checksums) > 0 {
		if !r.u.Interactive {
			return &exitError{code: 1, msg: fmt.Sprintf("%s pins checksums, which can't be checked for %s (not installed by gitopsify). Install the pinned version:\n  gitopsify provider install %s", r.cfgPath, p.Path, installRef)}
		}
		r.u.Warn("%s pins checksums, which can't be checked for %s (not installed by gitopsify)", r.cfgPath, p.Path)
	}
	if err := p.Verify(r.cfg.Spec.Provider.Checksums); err != nil {
		return &exitError{code: 1, msg: fmt.Sprintf("%v. Reinstall it with:\n  gitopsify provider install %s", err, installRef)}
	}
	r.p = p
	name := p.Name
	if p.Origin == provider.OriginFile {
		name = "" // a file can have any name; trust what it says it is
	}
	r.client = &provider.Client{Name: name, Path: p.Path, Log: r.log}
	d, err := r.client.Describe(ctx)
	if err != nil {
		return err
	}
	if !provider.ValidName(d.Name) {
		return fmt.Errorf("%s calls itself %q, which isn't a provider name", p.Path, d.Name)
	}
	r.d = d
	if pin != "" && p.Origin != provider.OriginInstalled && !sameVersion(d.Version, pin) {
		r.u.Warn("using %s %s from %s; %s pins %s", p.Name, d.Version, p.Path, r.cfgPath, pin)
	}
	r.cfg.Spec.Provider.Name = d.Name
	r.u.Step("provider %s %s", d.Name, d.Version)
	return nil
}

func (r *exportRun) log(l provider.LogLine) {
	msg := clean(l.String())
	switch l.Level {
	case "INFO":
		r.u.Step("%s", msg)
	case "WARN", "ERROR":
		r.u.Warn("%s", msg)
	default:
		if os.Getenv("GITOPSIFY_DEBUG") != "" {
			r.u.Step("%s", msg)
		}
	}
}

// applyFlags lays the flags over the export file.
func (r *exportRun) applyFlags(cmd *cobra.Command) error {
	c := &r.cfg.Spec
	set := func(name string) bool { return cmd.Flags().Changed(name) }
	if set("partition") {
		c.Select.Partitions = r.f.partitions
	}
	if set("kind") {
		c.Select.Kinds = nil
		for _, k := range r.f.kinds {
			kind, ok := r.kind(k)
			if !ok {
				return fmt.Errorf("--kind %s: %s has no such kind (it has %s)", k, r.d.Name, strings.Join(r.kindNames(), ", "))
			}
			c.Select.Kinds = append(c.Select.Kinds, kind)
		}
	}
	if set("include") {
		c.Select.Include = r.f.include
	}
	if set("exclude") {
		c.Select.Exclude = r.f.exclude
	}
	if set("no-deps") {
		c.Select.NoDeps = r.f.noDeps
	}
	if set("prune") {
		c.Select.Prune = r.f.prune
	}
	if set("to") {
		c.Output.Formats = r.f.to
	}
	if set("no-adopt") {
		adopt := !r.f.noAdopt
		c.Output.Adopt = &adopt
	}
	if set("secret-values") {
		c.Output.SecretValues = r.f.secretValues
	}
	if set("output") {
		c.Output.Dir = r.o.output
		r.outputFlag = true
	}
	for _, k := range c.Select.Kinds {
		if _, ok := r.kind(k); !ok {
			return fmt.Errorf("%s: %s has no kind %s", r.cfgPath, r.d.Name, k)
		}
	}
	for _, f := range c.Output.Formats {
		if (f == config.FormatTerraform && r.d.Terraform == nil) || (f == config.FormatCrossplane && r.d.Crossplane == nil) {
			return fmt.Errorf("%s can't produce %s", r.d.Name, f)
		}
	}
	check := *r.cfg
	check.Default()
	return check.Validate()
}

func (r *exportRun) kind(name string) (string, bool) {
	for _, k := range r.d.Kinds {
		if strings.EqualFold(k.Name, name) || strings.EqualFold(k.Plural, name) {
			return k.Name, true
		}
	}
	return "", false
}

func (r *exportRun) kindNames() []string {
	var out []string
	for _, k := range r.d.Kinds {
		out = append(out, k.Name)
	}
	return out
}

// connect resolves every instance's settings and checks it.
func (r *exportRun) connect(ctx context.Context) error {
	c := &r.cfg.Spec
	firstRun := len(c.Instances) == 0
	if firstRun {
		c.Instances = []config.ExportInstance{{Name: "default"}}
	}
	sets := map[string]map[string]string{}
	for _, s := range r.f.sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--set %q: want name=value", s)
		}
		inst := ""
		if i, n, found := strings.Cut(k, "."); found {
			inst, k = i, n
		}
		if inst == "" {
			if len(c.Instances) > 1 {
				return fmt.Errorf("--set %s: there are several instances; use --set <instance>.%s=...", k, k)
			}
			inst = c.Instances[0].Name
		}
		if !r.hasSetting(k) {
			return fmt.Errorf("--set %s: %s has no setting %s (see gitopsify provider info %s)", k, r.d.Name, k, r.d.Name)
		}
		if sets[inst] == nil {
			sets[inst] = map[string]string{}
		}
		sets[inst][k] = v
	}
	for name := range sets {
		found := false
		for _, in := range c.Instances {
			found = found || in.Name == name
		}
		if !found {
			return fmt.Errorf("--set: no instance named %s", name)
		}
	}

	for i := range c.Instances {
		in := &c.Instances[i]
		settings, err := r.resolveSettings(in, sets[in.Name], len(c.Instances))
		if err != nil {
			return err
		}
		if firstRun && r.u.Interactive {
			name := in.Name
			if u := settings["url"]; u != "" {
				name = instanceNameFromURL(u)
			}
			if err := r.u.Input("Name for this instance", "Used in directory names, such as terraform/"+name+"/<"+r.d.Partition.Singular+">.", name, &name, func(s string) error {
				if !config.ValidInstanceName(s) {
					return fmt.Errorf("lowercase letters, digits and dashes")
				}
				return nil
			}); err != nil {
				return err
			}
			in.Name = name
		}
		ri := &runInstance{cfg: in, settings: settings}
		r.u.Step("connecting to %s", in.Name)
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		res, err := r.client.Check(cctx, sdk.Request{Instance: in.Name, Settings: settings})
		cancel()
		if err != nil {
			if cctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("%s: the provider didn't answer check within 2 minutes", in.Name)
			}
			return providerError(in.Name, err)
		}
		res.Target, res.Identity = clean(res.Target), clean(res.Identity)
		for i := range res.Partitions {
			res.Partitions[i].Name = clean(res.Partitions[i].Name)
		}
		ri.check = res
		who := ""
		if res.Identity != "" {
			who = " as " + res.Identity
		}
		r.u.Success("%s: %s%s, %d %s", in.Name, res.Target, who, len(res.Partitions), plural(len(res.Partitions), r.d.Partition))
		for _, w := range res.Warnings {
			r.u.Warn("%s: %s", in.Name, clean(w))
		}
		for _, k := range res.Kinds {
			if !k.OK {
				r.u.Warn("%s: can't read %s: %s", in.Name, clean(k.Kind), clean(k.Message))
			}
		}
		r.insts = append(r.insts, ri)
	}
	return nil
}

func (r *exportRun) hasSetting(name string) bool {
	for _, s := range r.d.Settings {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (r *exportRun) resolveSettings(in *config.ExportInstance, sets map[string]string, instances int) (map[string]string, error) {
	out := map[string]string{}
	where := r.cfgPath
	if where == "" {
		where = config.DefaultExportFile(r.d.Name)
	}
	for _, s := range r.d.Settings {
		if v, ok := sets[s.Name]; ok {
			out[s.Name] = v
			continue
		}
		if cv, ok := in.Settings[s.Name]; ok {
			if cv.File != "" && r.fromFile && !filepath.IsAbs(config.ExpandHome(cv.File)) {
				cv.File = filepath.Join(filepath.Dir(r.cfgPath), cv.File)
			}
			if s.Secret && cv.Literal() && cv.Value != "" {
				return nil, fmt.Errorf("%s: instance %s: setting %s is a secret; write it as {env: %s} or {file: path}, not as a value", where, in.Name, s.Name, firstNonEmpty(s.Env, "VARIABLE"))
			}
			v, err := cv.Resolve()
			if err != nil {
				return nil, fmt.Errorf("%s: instance %s: setting %s: %w", where, in.Name, s.Name, err)
			}
			out[s.Name] = v
			continue
		}
		if s.Env != "" {
			if v, ok := os.LookupEnv(s.Env); ok && v != "" {
				if instances > 1 {
					r.u.Warn("instance %s: %s comes from $%s, which every instance without its own setting shares", in.Name, s.Name, s.Env)
				}
				out[s.Name] = v
				r.fromEnv[in.Name+"/"+s.Name] = s.Env
				if instances > 1 {
					r.u.Step("%s: %s from $%s", in.Name, s.Name, s.Env)
				} else {
					r.u.Step("%s from $%s", s.Name, s.Env)
				}
				continue
			}
		}
		if s.Required && r.u.Interactive {
			v := s.Default
			title := firstNonEmpty(s.Title, s.Name)
			desc := s.Description
			if s.Env != "" {
				desc += " (next time, set " + s.Env + " to skip this question)"
			}
			validate := func(x string) error {
				if strings.TrimSpace(x) == "" {
					return fmt.Errorf("required")
				}
				return nil
			}
			var err error
			if s.Secret {
				err = r.u.Password(title, desc, &v, validate)
				r.typedSecret[s.Name] = true
			} else {
				err = r.u.Input(title, desc, s.Example, &v, validate)
			}
			if err != nil {
				return nil, err
			}
			out[s.Name] = strings.TrimSpace(v)
			r.asked = true
			continue
		}
		if s.Default != "" {
			out[s.Name] = s.Default
			continue
		}
		if s.Required {
			hint := "--set " + s.Name + "=..."
			if s.Secret {
				hint = "the environment"
			}
			if s.Env != "" {
				hint = "$" + s.Env + " or " + hint
			}
			return nil, fmt.Errorf("instance %s needs setting %s (%s): set %s, or add it to %s", in.Name, s.Name, s.Description, hint, where)
		}
	}
	return out, nil
}

// choose settles partitions, kinds, objects, formats, adoption and the
// output directory: from flags and the file, else by asking.
func (r *exportRun) choose(ctx context.Context) error {
	c := &r.cfg.Spec
	u := r.u
	// Partitions.
	if len(c.Select.Partitions) == 0 && u.Interactive && !r.fromFile {
		var opts []ui.Option
		seen := map[string]bool{}
		var all []string
		for _, ri := range r.insts {
			for i := range ri.check.Partitions {
				p := &ri.check.Partitions[i]
				s := iac.PartitionSlug(p)
				if seen[s] {
					continue
				}
				seen[s] = true
				label := p.Name
				if len(r.insts) > 1 {
					label = ri.cfg.Name + ": " + p.Name
				}
				opts = append(opts, ui.Option{Label: fmt.Sprintf("%s (%s)", label, s), Value: s})
				all = append(all, s)
			}
		}
		if len(opts) > 1 {
			selected := append([]string(nil), all...)
			if err := u.MultiSelect("Which "+r.d.Partition.Plural+"?", "Each one becomes its own Terraform workspace and Kustomization.", opts, &selected); err != nil {
				return err
			}
			if len(selected) == 0 {
				return fmt.Errorf("no %s selected", r.d.Partition.Plural)
			}
			if len(selected) < len(all) {
				c.Select.Partitions = selected
			}
			r.asked = true
		}
	}
	hits := map[string]bool{}
	for _, ri := range r.insts {
		for _, pat := range c.Select.Partitions {
			if ids, err := iac.MatchPartitions(ri.check.Partitions, []string{pat}); err == nil {
				hits[pat] = true
				ri.ids = appendNew(ri.ids, ids...)
			}
		}
		if len(c.Select.Partitions) == 0 {
			ri.ids, _ = iac.MatchPartitions(ri.check.Partitions, nil)
		}
	}
	allInsts := r.insts
	if len(c.Select.Partitions) > 0 {
		var kept []*runInstance
		for _, ri := range r.insts {
			if len(ri.ids) == 0 {
				r.u.Step("%s: no %s matches; leaving it out", ri.cfg.Name, r.d.Partition.Singular)
				continue
			}
			kept = append(kept, ri)
		}
		r.insts = kept
	}
	for _, pat := range c.Select.Partitions {
		if !hits[pat] {
			var names []string
			for _, ri := range allInsts {
				for i := range ri.check.Partitions {
					names = append(names, iac.PartitionSlug(&ri.check.Partitions[i]))
				}
			}
			return fmt.Errorf("no %s matches %q (there are: %s)", r.d.Partition.Singular, pat, strings.Join(names, ", "))
		}
	}

	// Kinds.
	unreadable := map[string]bool{}
	for _, ri := range r.insts {
		for _, k := range ri.check.Kinds {
			if !k.OK {
				unreadable[k.Kind] = true
			}
		}
	}
	if len(c.Select.Kinds) == 0 {
		var defaults []string
		var opts []ui.Option
		for _, k := range r.d.Kinds {
			label := firstNonEmpty(k.Plural, k.Name)
			if k.Description != "" {
				rs := []rune(k.Description)
				label = strings.ToUpper(string(rs[0])) + string(rs[1:])
			}
			if unreadable[k.Name] {
				label += " (can't be read)"
			}
			opts = append(opts, ui.Option{Label: label, Value: k.Name})
			if !k.Optional && r.supportsKind(k) && (!unreadable[k.Name] || !u.Interactive || r.fromFile) {
				defaults = append(defaults, k.Name)
			}
		}
		if u.Interactive && !r.fromFile {
			selected := defaults
			if err := u.MultiSelect("What do you want to export?", "Kinds of objects", opts, &selected); err != nil {
				return err
			}
			if len(selected) == 0 {
				return fmt.Errorf("no kinds selected")
			}
			c.Select.Kinds = selected
			r.asked = true
		} else {
			c.Select.Kinds = defaults
		}
	}

	// Formats.
	var supported []string
	if r.d.Terraform != nil {
		supported = append(supported, config.FormatTerraform)
	}
	if r.d.Crossplane != nil {
		supported = append(supported, config.FormatCrossplane)
	}
	if len(c.Output.Formats) == 0 {
		selected := append([]string(nil), supported...)
		if u.Interactive && !r.fromFile && len(supported) > 1 {
			opts := []ui.Option{{Label: "Terraform / OpenTofu: one root module per workspace", Value: config.FormatTerraform},
				{Label: "Crossplane: managed resources in one Kustomization per workspace", Value: config.FormatCrossplane}}
			if err := u.MultiSelect("Write which formats?", "", opts, &selected); err != nil {
				return err
			}
			if len(selected) == 0 {
				return fmt.Errorf("no format selected")
			}
			r.asked = true
		}
		c.Output.Formats = selected
	}

	// Adopt or replicate.
	if c.Output.Adopt == nil && u.Interactive && !r.fromFile {
		choice := "adopt"
		if err := u.Select("Should the configuration manage these objects, or create copies?", "", []ui.Option{
			{Label: "Manage them (adopt): import what was read; the first plan changes nothing", Value: "adopt"},
			{Label: "Create them elsewhere (replicate): for another instance, nothing is imported", Value: "replicate"},
		}, &choice); err != nil {
			return err
		}
		adopt := choice == "adopt"
		c.Output.Adopt = &adopt
		r.asked = true
	}

	// Pick objects.
	if len(c.Select.Include) == 0 && u.Interactive && !r.fromFile {
		choice := "all"
		if err := u.Select("Export everything, or pick objects?", "", []ui.Option{
			{Label: "Everything of these kinds", Value: "all"},
			{Label: "Let me pick (what they reference comes along)", Value: "pick"},
		}, &choice); err != nil {
			return err
		}
		if choice == "pick" {
			if err := r.pick(ctx); err != nil {
				return err
			}
			r.asked = true
		}
	}

	// Output directory.
	if c.Output.Dir == "" {
		c.Output.Dir = r.d.Name
		if u.Interactive && !r.fromFile {
			if err := u.Input("Output directory", "", c.Output.Dir, &c.Output.Dir, nil); err != nil {
				return err
			}
			r.asked = true
		}
	}
	return nil
}

func (r *exportRun) supportsKind(k sdk.Kind) bool {
	return (k.Terraform != nil && r.d.Terraform != nil) || (k.Crossplane != nil && r.d.Crossplane != nil)
}

// pick lists the objects in scope (cheaply) and lets the user choose.
func (r *exportRun) pick(ctx context.Context) error {
	var opts []ui.Option
	var summaries []*sdk.Inventory
	for _, ri := range r.insts {
		r.u.Step("listing %s objects", ri.cfg.Name)
		lctx, cancel := context.WithTimeout(ctx, r.f.timeout)
		inv, err := r.client.Discover(lctx, sdk.Request{Instance: ri.cfg.Name, Settings: ri.settings, Detail: sdk.DetailSummary,
			Scope: sdk.Scope{Partitions: ri.ids, Kinds: r.cfg.Spec.Select.Kinds}}, nil)
		cancel()
		if err != nil {
			return providerError(ri.cfg.Name, err)
		}
		summaries = append(summaries, inv)
		for i := range inv.Resources {
			res := &inv.Resources[i]
			addr := iac.Address(inv, res)
			label := addr
			if !strings.HasSuffix(addr, "/"+res.Name) {
				label += "  " + r.u.Dim(res.Name)
			}
			if len(r.insts) > 1 {
				label = ri.cfg.Name + ": " + label
			}
			opts = append(opts, ui.Option{Label: label, Value: addr})
		}
	}
	if len(opts) == 0 {
		return fmt.Errorf("nothing to export in that scope")
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Label < opts[j].Label })
	var selected []string
	if err := r.u.MultiSelect("Which objects? (type to filter)", "What they reference (folders, ...) comes along.", opts, &selected); err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("nothing selected")
	}
	r.cfg.Spec.Select.Include = selected
	// Read only what was picked and what it needs.
	for i, ri := range r.insts {
		picked := iac.Select(summaries[i], iac.Selection{Include: selected})
		for _, res := range picked.Resources {
			ri.keys = append(ri.keys, res.Key)
		}
	}
	return nil
}

func (r *exportRun) discover(ctx context.Context) error {
	c := &r.cfg.Spec
	sel := iac.Selection{Include: c.Select.Include, Exclude: c.Select.Exclude, NoDeps: c.Select.NoDeps}
	selecting := len(sel.Include) > 0 || len(sel.Exclude) > 0
	prev, _ := iac.LoadState(r.outDir())
	var warnings []string
	for _, ri := range r.insts {
		if len(ri.keys) > 0 && prev != nil {
			// Read what was exported before too, to tell what still exists.
			for k, rs := range prev.Resources {
				if key, ok := strings.CutPrefix(k, ri.cfg.Name+"/"); ok && !rs.Gone && !strings.Contains(key, "#") {
					ri.keys = appendNew(ri.keys, key)
				}
			}
		}
		r.u.Step("reading %s", ri.cfg.Name)
		n := 0
		start := time.Now()
		inv, err := r.client.Discover(ctx, sdk.Request{Instance: ri.cfg.Name, Settings: ri.settings,
			Scope: sdk.Scope{Partitions: ri.ids, Kinds: c.Select.Kinds, Keys: ri.keys}}, func(sdk.Resource) { n++ })
		if err == nil {
			// Kinds check couldn't read are incomplete, even if discover
			// returned nothing for them without a warning.
			for _, k := range ri.check.Kinds {
				if !k.OK && contains(c.Select.Kinds, k.Kind) {
					inv.Warnings = append(inv.Warnings, sdk.Warning{Kind: k.Kind, Message: firstNonEmpty(k.Message, "can't be read")})
				}
			}
		}
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("%s: reading took longer than --timeout %s", ri.cfg.Name, r.f.timeout)
			}
			return providerError(ri.cfg.Name, err)
		}
		ri.inv = inv
		exporting := len(inv.Resources)
		if selecting {
			picked := iac.Select(inv, sel)
			ri.selected = map[string]bool{}
			for _, res := range picked.Resources {
				ri.selected[res.Key] = true
			}
			inv.Skipped = picked.Skipped
			exporting = len(ri.selected)
		}
		r.u.Success("%s: read %s in %s, selected %d", ri.cfg.Name, count(n, "object"), time.Since(start).Round(100*time.Millisecond), exporting)
		for _, w := range inv.Warnings {
			msg := clean(w.Message)
			if w.Kind != "" {
				msg = w.Kind + ": " + msg
			}
			if w.Partition != "" {
				msg = partitionLabel(inv, w.Partition) + ": " + msg
			}
			warnings = append(warnings, ri.cfg.Name+": "+msg)
		}
	}
	if len(warnings) == 0 {
		return nil
	}
	for _, w := range warnings {
		r.u.Warn("couldn't read %s", w)
	}
	if r.f.allowPartial {
		return nil
	}
	if r.u.Interactive {
		ok, err := r.u.Confirm("Export the rest?", "What couldn't be read keeps the files of the previous export (nothing is treated as deleted).", false)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return ui.ErrAborted
	}
	return &exitError{code: 3, msg: "the provider couldn't read everything (see above); fix the cause, or pass --allow-partial to export the rest and keep the previous files of what couldn't be read"}
}

func (r *exportRun) write() error {
	c := &r.cfg.Spec
	u := r.u
	adopt := c.Output.AdoptOrDefault()

	// Offer to write secret values the tool returned.
	if c.Output.SecretValues == "" {
		readable := 0
		for _, ri := range r.insts {
			for _, res := range ri.inv.Resources {
				walkSecrets(res.Attributes, func(s sdk.SecretValue) {
					if s.Value != nil {
						readable++
					}
				})
			}
		}
		c.Output.SecretValues = config.SecretValuesSkip
		if readable > 0 && u.Interactive && !r.fromFile {
			choice := config.SecretValuesSkip
			if err := u.Select(fmt.Sprintf("%d secret values were read (webhook URLs, tokens...). What should happen to them?", readable), "Either way they never go into tracked files.", []ui.Option{
				{Label: "Leave them out: they become Terraform variables and Kubernetes Secrets you fill in", Value: config.SecretValuesSkip},
				{Label: "Write them to git-ignored files next to the configuration", Value: config.SecretValuesLocal},
			}, &choice); err != nil {
				return err
			}
			c.Output.SecretValues = choice
			r.asked = true
		}
	}

	var instances []*iac.Instance
	for _, ri := range r.insts {
		instances = append(instances, &iac.Instance{Name: ri.cfg.Name, Settings: ri.settings, Target: ri.check.Target,
			Kinds: c.Select.Kinds, Inventory: ri.inv, Selected: ri.selected})
	}
	dir := r.outDir()
	prev, err := iac.LoadState(dir)
	if err != nil {
		return err
	}
	if prev != nil && prev.Provider != "" && prev.Provider != r.d.Name {
		return fmt.Errorf("%s holds an export of %s, not %s", dir, prev.Provider, r.d.Name)
	}
	res, err := iac.Build(iac.Input{Description: r.d, Instances: instances, Previous: prev, Options: iac.Options{
		Formats: c.Output.Formats, Adopt: adopt, SecretValues: c.Output.SecretValues,
		NamespacePrefix: c.Output.Crossplane.NamespacePrefix, ManagementPolicies: c.Output.Crossplane.ManagementPolicies,
		Backend: c.Output.Terraform.Backend, Prune: c.Select.Prune,
	}})
	if err != nil {
		return err
	}
	r.summary(res)
	if edited := res.HandEdited(dir); len(edited) > 0 {
		shown := edited
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(edited)-5))
		}
		u.Warn("%s changed since the last export and will be overwritten: %s (keep your own configuration in separate files)",
			count(len(edited), "generated file"), strings.Join(shown, ", "))
	}
	if r.f.dryRun {
		u.Println()
		u.Println("Dry run: nothing written. Files: " + fmt.Sprint(len(res.Files())) + " under " + dir)
		return nil
	}
	if u.Interactive {
		ok, err := u.Confirm(fmt.Sprintf("Write to %s?", dir), "", true)
		if err != nil {
			return err
		}
		if !ok {
			return ui.ErrAborted
		}
	}
	written, removed, err := res.Write(dir, r.o.force)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("wrote %d files to %s", len(written), dir)
	if len(removed) > 0 {
		msg += fmt.Sprintf(", removed %d stale ones", len(removed))
	}
	u.Success("%s", msg)
	if err := r.maybeSave(); err != nil {
		return err
	}
	r.nextSteps(res)
	return nil
}

func (r *exportRun) summary(res *iac.Result) {
	rep := &res.Report
	kinds := map[string]bool{}
	for _, w := range rep.Workspaces {
		for k := range w.Counts {
			kinds[k] = true
		}
	}
	cols := make([]string, 0, len(kinds))
	for k := range kinds {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	headers := append([]string{"WORKSPACE", "DIRECTORY"}, cols...)
	headers = append(headers, "GONE")
	var rows [][]string
	for _, w := range rep.Workspaces {
		title := w.Title
		if len(w.Incomplete) > 0 {
			title += " (incomplete)"
		}
		row := []string{title, w.Dir}
		for _, k := range cols {
			row = append(row, fmt.Sprint(w.Counts[k]))
		}
		row = append(row, fmt.Sprint(w.Gone))
		rows = append(rows, row)
	}
	r.u.Heading(fmt.Sprintf("%s in %s", count(rep.Total(), "object"), count(len(rep.Workspaces), "workspace")))
	r.u.Table(headers, rows)
	if n := len(rep.Skipped); n > 0 {
		r.u.Step("%s left out on purpose (managed elsewhere, or not mappable yet): see EXPORT_REPORT.md", count(n, "object"))
	}
	if n := rep.GoneTotal(); n > 0 {
		r.u.Warn("%s exported before no longer: Terraform gets removed blocks (forget, don't destroy)", count(n, "object"))
	}
	if n := len(rep.MissingSecrets); n > 0 {
		r.u.Warn("%s not in the output: listed in EXPORT_REPORT.md", count(n, "secret value"))
	}
	if n := len(rep.Large); n > 0 {
		r.u.Warn("%s over 512 KiB: apply with server-side apply", count(n, "Crossplane object"))
	}
}

func (r *exportRun) nextSteps(res *iac.Result) {
	c := &r.cfg.Spec
	rep := &res.Report
	u := r.u
	u.Heading("Next steps")
	u.Println("  Read " + filepath.Join(c.Output.Dir, "EXPORT_REPORT.md") + ", then:")
	if contains(c.Output.Formats, config.FormatTerraform) {
		first := ""
		for _, w := range rep.Workspaces {
			first = w.Dir
			break
		}
		envs := ""
		for _, s := range r.d.Settings {
			if s.Secret && s.Env != "" {
				envs += " " + s.Env + "=..."
			}
		}
		u.Println("  Terraform:   cd " + filepath.Join(c.Output.Dir, "terraform", first) + " && terraform init && " + strings.TrimSpace(envs+" terraform plan"))
		if n := len(rep.Variables); n > 0 {
			u.Println(fmt.Sprintf("               %s need values (TF_VAR_...): listed in EXPORT_REPORT.md", count(n, "secret variable")))
		}
	}
	if contains(c.Output.Formats, config.FormatCrossplane) {
		u.Println("  Crossplane:  kubectl apply -k " + filepath.Join(c.Output.Dir, "crossplane", "setup") + "   # then see crossplane/README.md")
	}
	if r.cfgPath != "" {
		u.Println("  Again / CI:  gitopsify export --config " + r.cfgPath + " --no-input")
	}
}

// maybeSave writes the export file when asked to, or offers to after a
// wizard run.
func (r *exportRun) maybeSave() error {
	save := r.f.save
	path := r.cfgPath
	if path == "" {
		path = config.DefaultExportFile(r.d.Name)
	}
	if !save && r.asked && r.u.Interactive {
		var err error
		save, err = r.u.Confirm("Save these choices to "+path+"?", "Commit it and run \"gitopsify export --no-input\" in CI to repeat this export. Secrets are saved as environment variable names, never values.", true)
		if err != nil {
			return err
		}
	}
	if !save {
		return nil
	}
	x := *r.cfg
	x.Default()
	c := &x.Spec
	// The file's dir is relative to the file; -o was relative to here.
	if r.outputFlag && !filepath.IsAbs(c.Output.Dir) {
		if abs, err := filepath.Abs(c.Output.Dir); err == nil {
			if fileDir, err := filepath.Abs(filepath.Dir(path)); err == nil {
				if rel, err := filepath.Rel(fileDir, abs); err == nil {
					c.Output.Dir = filepath.ToSlash(rel)
				}
			}
		}
	}
	if r.p.Origin == provider.OriginInstalled {
		if c.Provider.Source == "" {
			c.Provider.Source = r.p.Source
		}
		c.Provider.Version = r.p.Version
		if len(r.p.Checksums) > 0 {
			c.Provider.Checksums = r.p.Checksums
		} else if r.p.SHA256 != "" {
			c.Provider.Checksums = map[string]string{runtime.GOOS + "_" + runtime.GOARCH: r.p.SHA256}
		}
	}
	if sameSet(c.Select.Kinds, r.defaultKinds()) {
		c.Select.Kinds = nil // the provider's defaults, including ones it adds later
	}
	var needEnv []string
	for i := range c.Instances {
		in := &c.Instances[i]
		if in.Settings == nil {
			in.Settings = map[string]config.SettingValue{}
		}
		for _, s := range r.d.Settings {
			if _, ok := in.Settings[s.Name]; ok {
				continue
			}
			ri := r.instance(in.Name)
			if ri == nil {
				continue // left out of this run: keep what the file says
			}
			v := ri.settings[s.Name]
			switch {
			case s.Secret && s.Env != "":
				in.Settings[s.Name] = config.SettingValue{Env: s.Env}
				if r.typedSecret[s.Name] {
					needEnv = append(needEnv, s.Env)
				}
			case s.Secret:
			case r.fromEnv[in.Name+"/"+s.Name] != "":
				in.Settings[s.Name] = config.SettingValue{Env: r.fromEnv[in.Name+"/"+s.Name]}
			case v != "" && v != s.Default:
				in.Settings[s.Name] = config.SettingValue{Value: v}
			}
		}
	}
	comment := "# gitopsify export of " + r.d.Title + ". Reference: https://github.com/hayone1/gitopsify/blob/main/docs/providers.md\n"
	b, err := config.SaveExport(path, &x, r.loadedAs, config.ExportSchemaHeader, comment)
	if errors.Is(err, config.ErrNotInPlace) {
		// Never rewrite the other resources of a shared file: save the
		// export on its own.
		// Next to it, so that output.dir and file: settings, which are
		// relative to the file, keep their meaning.
		alt := filepath.Join(filepath.Dir(path), config.DefaultExportFile(r.d.Name))
		if filepath.Clean(alt) == filepath.Clean(path) || fileExists(alt) {
			return fmt.Errorf("saving to %s: %w; copy the export into it by hand, or remove %s and save again", path, err, alt)
		}
		r.u.Warn("%s can't be updated in place: %v. Saving %s instead (pass it with --config next time); the old export is still in %s, so remove it there (or move this one in)", path, err, alt, path)
		path = alt
		b, err = config.Encode(path, &x, config.ExportSchemaHeader, comment)
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	r.cfgPath = path
	r.u.Success("saved %s", path)
	for _, e := range needEnv {
		r.u.Step("set %s wherever this export runs again (it reads the secret from there)", e)
	}
	return nil
}

func providerError(inst string, err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		var se *sdk.Error
		if errors.As(pe.Err, &se) {
			msg := fmt.Sprintf("%s: %s", inst, clean(se.Message))
			if se.Hint != "" {
				msg += "\n  " + clean(se.Hint)
			}
			return &exitError{code: 1, msg: msg}
		}
	}
	return fmt.Errorf("%s: %w", inst, err)
}

func instanceNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "default"
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" {
		return "local"
	}
	name := strings.Split(host, ".")[0]
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "-")
	if !config.ValidInstanceName(s) {
		return "default"
	}
	return s
}

func plural(n int, noun sdk.Noun) string {
	if n == 1 {
		return firstNonEmpty(noun.Singular, "partition")
	}
	return firstNonEmpty(noun.Plural, "partitions")
}

func partitionLabel(inv *sdk.Inventory, id string) string {
	for _, p := range inv.Partitions {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}

func appendNew(list []string, items ...string) []string {
	for _, it := range items {
		if !contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func walkSecrets(v any, fn func(sdk.SecretValue)) {
	switch t := v.(type) {
	case sdk.SecretValue:
		fn(t)
	case map[string]any:
		for _, e := range t {
			walkSecrets(e, fn)
		}
	case []any:
		for _, e := range t {
			walkSecrets(e, fn)
		}
	}
}

// count writes "1 object" or "3 objects".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (r *exportRun) defaultKinds() []string {
	var out []string
	for _, k := range r.d.Kinds {
		if !k.Optional && r.supportsKind(k) {
			out = append(out, k.Name)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

// outDir is the output directory: relative to the export file when it comes
// from one, relative to the working directory when given with -o.
func (r *exportRun) outDir() string {
	dir := r.cfg.Spec.Output.Dir
	if r.fromFile && !r.outputFlag && dir != "" && !filepath.IsAbs(dir) {
		return filepath.Join(filepath.Dir(r.cfgPath), dir)
	}
	return dir
}

// sameVersion compares versions with or without a leading v.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// clean drops control characters (terminal escapes) from text a provider
// sent.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
			return r
		}
		return -1
	}, s)
}

// instance returns the run instance with a name, nil if it isn't in this run.
func (r *exportRun) instance(name string) *runInstance {
	for _, ri := range r.insts {
		if ri.cfg.Name == name {
			return ri
		}
	}
	return nil
}
