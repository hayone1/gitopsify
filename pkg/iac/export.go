// Package iac turns what a provider read from a tool into Terraform and
// Crossplane configuration: workspaces per instance and partition, stable
// names, adoption (import blocks, external names), secrets kept out of Git,
// and re-exports that never delete what they stop seeing.
package iac

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

// Options shape the output.
type Options struct {
	Formats      []string
	Adopt        bool
	SecretValues string
	// NamespacePrefix starts Crossplane namespaces (default: provider name).
	NamespacePrefix    string
	ManagementPolicies []string
	Backend            *config.TerraformBackend
	// Prune stops managing objects exported before that the selection
	// leaves out (they get removed blocks). Without it they stay.
	Prune bool
}

// Instance is one connection's answer.
type Instance struct {
	Name string
	// Settings are the resolved connection settings.
	Settings map[string]string
	// Target is what check said the instance is.
	Target string
	// Kinds are the kinds in scope (complete or not); empty means all.
	// Resources of other kinds are ignored.
	Kinds []string
	// Inventory is what was read: the partitions in scope and their
	// resources. Resources outside the partitions or kinds are ignored.
	Inventory *sdk.Inventory
	// Selected holds the keys the selection picked (nil: all). Objects
	// exported before stay exported even when not selected, unless
	// Options.Prune.
	Selected map[string]bool
}

func (in *Instance) hasKind(k string) bool { return len(in.Kinds) == 0 || contains(in.Kinds, k) }

// Input is everything an export needs.
type Input struct {
	Description *sdk.Description
	Instances   []*Instance
	Options     Options
	// Previous is the state of the last export into the same directory.
	Previous *State
}

// Result is a rendered export, ready to write.
type Result struct {
	State *State
	// Report summarises the export for people.
	Report Report

	units    map[unitKey]*unit
	previous *State
	desc     *sdk.Description
}

type unitKey struct{ format, ws, kind string }

type unit struct {
	files map[string][]byte
	// secret files are written with mode 0600.
	secret map[string]bool
}

func (u *unit) add(p string, b []byte, secret bool) {
	u.files[p] = b
	if secret {
		u.secret[p] = true
	}
}

// workspace is one instance × partition.
type workspace struct {
	id        string
	inst      *Instance
	partition *sdk.Partition // nil for instance-wide objects
	dir       string
	namespace string
	resources []*resource
	// incomplete holds kinds that weren't read completely; "*" means all.
	incomplete map[string]bool
	// unannounced is a partition only a warning named: its attributes are
	// unknown, so the workspace's own files (provider settings) stay as
	// the last export wrote them.
	unannounced bool
	// vars names Terraform variables: one module per workspace, so names
	// are unique across its kinds' files.
	vars *namer
	// gone holds resources exported before that aren't any more, by kind.
	gone map[string][]*goneResource
}

func (w *workspace) title() string {
	if w.partition == nil {
		return w.inst.Name + " (instance-wide)"
	}
	return w.inst.Name + " / " + w.partition.Name
}

func (w *workspace) isIncomplete(kind string) bool { return w.incomplete["*"] || w.incomplete[kind] }

// declared reports whether a resource is in the output: its kind is
// written this time, or it was exported before (an incomplete kind keeps
// the last export's file). References to others would name nothing.
func declared(t *resource, prev *State) bool {
	if !t.ws.isIncomplete(t.kind.Name) {
		return true
	}
	if prev == nil {
		return false
	}
	ps, ok := prev.Resources[stateKey(t.ws.inst.Name, t.r.Key)]
	return ok && !ps.Gone
}

type resource struct {
	r     *sdk.Resource
	kind  *sdk.Kind
	ws    *workspace
	state *ResourceState
}

type goneResource struct {
	key   string
	state *ResourceState
}

func stateKey(inst, key string) string { return inst + "/" + key }

func wsID(inst, partition string) string { return inst + "/" + partition }

// Build renders the export.
func Build(in Input) (*Result, error) {
	d := in.Description
	opts := in.Options
	if opts.NamespacePrefix == "" {
		opts.NamespacePrefix = d.Name
	}
	in.Options = opts
	st := cloneState(in.Previous, d.Name)
	res := &Result{State: st, units: map[unitKey]*unit{}, previous: in.Previous, desc: d}
	if res.previous == nil {
		res.previous = NewState(d.Name)
	}

	workspaces, byKey, err := plan(in, st)
	if err != nil {
		return nil, err
	}
	rep := &res.Report
	rep.Provider = d.Title
	rep.Adopt = opts.Adopt
	rep.Formats = opts.Formats
	for _, inst := range in.Instances {
		rep.Instances = append(rep.Instances, InstanceReport{Name: inst.Name, Target: inst.Target})
		for _, w := range inst.Inventory.Warnings {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s", inst.Name, describeWarning(w)))
		}
		for _, s := range inst.Inventory.Skipped {
			where := inst.Name
			if s.Partition != "" {
				where += " / " + partitionName(inst.Inventory, s.Partition)
			}
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s %q (%s): %s", s.Kind, s.Name, where, s.Reason))
		}
	}

	for _, f := range opts.Formats {
		switch f {
		case config.FormatTerraform:
			if d.Terraform == nil {
				return nil, fmt.Errorf("%s can't produce Terraform", d.Name)
			}
			if err := renderTerraform(res, in, workspaces, byKey); err != nil {
				return nil, err
			}
		case config.FormatCrossplane:
			if d.Crossplane == nil {
				return nil, fmt.Errorf("%s can't produce Crossplane", d.Name)
			}
			if err := renderCrossplane(res, in, workspaces, byKey); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown format %q", f)
		}
	}
	for _, w := range workspaces {
		counts := map[string]int{}
		for _, r := range w.resources {
			counts[r.kind.Name]++
			for _, n := range r.r.Notes {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s %q (%s): %s", r.kind.Name, r.r.Name, w.title(), n))
			}
		}
		gone := 0
		for _, g := range w.gone {
			gone += len(g)
		}
		var incomplete []string
		for k := range w.incomplete {
			incomplete = append(incomplete, k)
		}
		sort.Strings(incomplete)
		rep.Workspaces = append(rep.Workspaces, WorkspaceReport{Title: w.title(), Dir: w.dir, Namespace: w.namespace,
			Counts: counts, Gone: gone, Incomplete: incomplete})
	}
	renderReport(res, in)
	return res, nil
}

// plan assigns workspaces and names, and finds resources that are gone.
func plan(in Input, st *State) ([]*workspace, map[string]*resource, error) {
	d := in.Description
	var workspaces []*workspace
	wsByID := map[string]*workspace{}
	byKey := map[string]*resource{}
	read := map[string]map[string]bool{}

	dirNamer := newNamer("-")
	nsNamer := newNamer("-")
	for _, w := range st.Workspaces {
		dirNamer.reserve(w.Dir)
		if w.Namespace != "" {
			nsNamer.reserve(w.Namespace)
		}
	}
	getWS := func(inst *Instance, partitionID string) *workspace {
		id := wsID(inst.Name, partitionID)
		if w, ok := wsByID[id]; ok {
			return w
		}
		w := &workspace{id: id, inst: inst, incomplete: map[string]bool{}, gone: map[string][]*goneResource{}}
		for i := range inst.Inventory.Partitions {
			if inst.Inventory.Partitions[i].ID == partitionID {
				w.partition = &inst.Inventory.Partitions[i]
			}
		}
		if w.partition == nil && partitionID != "" {
			w.partition = &sdk.Partition{ID: partitionID, Name: partitionID}
			w.unannounced = true
		}
		if prev, ok := st.Workspaces[id]; ok {
			w.dir, w.namespace = prev.Dir, prev.Namespace
		}
		if w.dir == "" {
			seg := "_instance"
			if w.partition != nil {
				seg = fileSegment(w.partition.Name, "p-"+shortHash(id))
			}
			w.dir = dirNamer.assign(inst.Name+"/"+seg, id)
		}
		if w.namespace == "" {
			part := "instance"
			if w.partition != nil {
				part = slug(w.partition.Name, "-")
				if part == "" {
					part = "p-" + shortHash(id)
				}
			}
			base := in.Options.NamespacePrefix
			if inst.Name != "default" {
				base += "-" + inst.Name
			}
			w.namespace = nsNamer.assign(truncate(base+"-"+part, 56, "-"), id)
		}
		st.Workspaces[id] = &WorkspaceState{Dir: w.dir, Namespace: w.namespace}
		wsByID[id] = w
		workspaces = append(workspaces, w)
		return w
	}

	for _, inst := range in.Instances {
		// Every partition in scope gets a workspace, even an empty one, so
		// resources that disappeared from it are noticed.
		for _, p := range inst.Inventory.Partitions {
			getWS(inst, p.ID)
		}
		partitions := map[string]bool{"": true}
		for _, p := range inst.Inventory.Partitions {
			partitions[p.ID] = true
		}
		read[inst.Name] = map[string]bool{}
		for i := range inst.Inventory.Resources {
			r := &inst.Inventory.Resources[i]
			k, ok := d.Kind(r.Kind)
			if !ok {
				return nil, nil, fmt.Errorf("%s: resource %s has kind %q, which the provider doesn't describe", inst.Name, r.Key, r.Kind)
			}
			// Only what is in scope: a resource of another kind or partition
			// would rewrite files this export doesn't own.
			if !inst.hasKind(r.Kind) || !partitions[r.Partition] {
				continue
			}
			sk := stateKey(inst.Name, r.Key)
			if read[inst.Name][r.Key] {
				return nil, nil, fmt.Errorf("%s: resource key %s is used twice", inst.Name, r.Key)
			}
			read[inst.Name][r.Key] = true
			rs := st.Resources[sk]
			exportedBefore := rs != nil && !rs.Gone
			selected := inst.Selected == nil || inst.Selected[r.Key]
			if !selected && (!exportedBefore || in.Options.Prune) {
				continue // not selected: gone below if it was exported before
			}
			if in.Options.Adopt && r.ImportID == "" && !exportedBefore {
				// Without an import there is nothing to adopt: the first
				// apply would create a second one.
				inst.Inventory.Skipped = append(inst.Inventory.Skipped[:len(inst.Inventory.Skipped):len(inst.Inventory.Skipped)],
					sdk.Skipped{Kind: r.Kind, Partition: r.Partition, Name: r.Name, Reason: "the provider gave no import ID, so adopting it would create a second one"})
				continue
			}
			w := getWS(inst, r.Partition)
			if rs != nil && (rs.Workspace != w.id || rs.Kind != r.Kind) {
				oldType, _, _ := strings.Cut(rs.Terraform, ".")
				oldKind, _ := d.Kind(rs.Kind)
				switch {
				case rs.Workspace == w.id && k.Terraform != nil && oldType == k.Terraform.Type:
					// Another kind, the same Terraform type: same address.
					if oldKind == nil || oldKind.Crossplane == nil || k.Crossplane == nil || oldKind.Crossplane.Kind != k.Crossplane.Kind {
						rs.Crossplane = ""
					}
					rs.Kind = r.Kind
				default:
					// The object moved (another partition or type): its old
					// address gets a removed block, it gets a new one.
					old := *rs
					old.Gone = true
					st.Retired = append(st.Retired, &old)
					rs = nil
				}
			}
			if rs == nil {
				rs = &ResourceState{Kind: r.Kind, Workspace: w.id}
				st.Resources[sk] = rs
			}
			rs.Gone = false
			res := &resource{r: r, kind: k, ws: w, state: rs}
			w.resources = append(w.resources, res)
			byKey[sk] = res
		}
		// Workspaces exported before stay in play even when nothing of
		// theirs was read now (everything deleted): what they held is gone.
		for _, id := range sortedMapKeys(st.Workspaces) {
			if part, ok := strings.CutPrefix(id, inst.Name+"/"); ok && (part == "" || partitions[part]) {
				getWS(inst, part)
			}
		}
		// Incomplete scopes. A warning about a kind the provider doesn't
		// describe, or a partition that is neither announced nor exported
		// before (a typo, a name instead of an ID), covers more rather
		// than less: what wasn't read must not look deleted.
		known := map[string]bool{}
		for id := range st.Workspaces {
			known[id] = true
		}
		for _, warn := range inst.Inventory.Warnings {
			if warn.Kind != "" {
				if _, ok := d.Kind(warn.Kind); !ok {
					warn.Kind = ""
				}
			}
			if warn.Partition != "" && !partitions[warn.Partition] && !known[wsID(inst.Name, warn.Partition)] {
				warn.Partition = ""
			}
			switch {
			case warn.Partition == "" && warn.Kind == "":
				for _, w := range workspaces {
					if w.inst == inst {
						w.incomplete["*"] = true
					}
				}
			case warn.Partition == "":
				for _, w := range workspaces {
					if w.inst == inst {
						w.incomplete[warn.Kind] = true
					}
				}
			default:
				w := getWS(inst, warn.Partition)
				kind := warn.Kind
				if kind == "" || !partitions[warn.Partition] {
					kind = "*" // not even announced: nothing in it was read
				}
				w.incomplete[kind] = true
			}
		}
	}
	for _, w := range workspaces {
		sort.Slice(w.resources, func(i, j int) bool { return w.resources[i].r.Key < w.resources[j].r.Key })
	}

	// Resources exported before, in a scope read completely now, that are
	// missing (or pruned): gone. They keep their names (and get removed
	// blocks).
	keys := make([]string, 0, len(st.Resources))
	for k := range st.Resources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, sk := range keys {
		rs := st.Resources[sk]
		if _, present := byKey[sk]; present {
			continue
		}
		w, ok := wsByID[rs.Workspace]
		if !ok || !w.inst.hasKind(rs.Kind) || w.isIncomplete(rs.Kind) {
			continue // out of scope this time: left as it is
		}
		rs.Gone = true
		w.gone[rs.Kind] = append(w.gone[rs.Kind], &goneResource{key: strings.TrimPrefix(sk, w.inst.Name+"/"), state: rs})
	}
	for _, rs := range st.Retired {
		w, ok := wsByID[rs.Workspace]
		if !ok || !w.inst.hasKind(rs.Kind) || w.isIncomplete(rs.Kind) {
			continue
		}
		w.gone[rs.Kind] = append(w.gone[rs.Kind], &goneResource{state: rs})
	}

	// Names: reuse recorded ones, then give new resources unique names.
	for _, w := range workspaces {
		tfNamers := map[string]*namer{}
		k8sNamers := map[string]*namer{}
		files := newNamer("-")
		tfNamer := func(t string) *namer {
			if tfNamers[t] == nil {
				tfNamers[t] = newNamer("_")
			}
			return tfNamers[t]
		}
		k8sNamer := func(k string) *namer {
			if k8sNamers[k] == nil {
				k8sNamers[k] = newNamer("-")
			}
			return k8sNamers[k]
		}
		for _, rs := range append(stateList(st), st.Retired...) {
			if rs.Workspace != w.id {
				continue
			}
			if t, n, ok := strings.Cut(rs.Terraform, "."); ok {
				tfNamer(t).reserve(n)
			}
			if k, ok := d.Kind(rs.Kind); ok && k.Crossplane != nil && rs.Crossplane != "" {
				k8sNamer(k.Crossplane.Kind).reserve(rs.Crossplane)
			}
			for _, f := range rs.Files {
				files.reserve(f)
			}
		}
		// claimed maps each written path (lower case) to its resource: two
		// resources never share a file, even if an older state says so.
		claimed := map[string]string{}
		for _, r := range w.resources {
			if k := r.kind.Terraform; k != nil {
				if t, _, _ := strings.Cut(r.state.Terraform, "."); t != k.Type {
					if r.state.Terraform != "" {
						// The provider maps this object to another type now:
						// forget the old address instead of destroying it.
						old := &ResourceState{Kind: r.state.Kind, Workspace: w.id, Terraform: r.state.Terraform, Gone: true}
						st.Retired = append(st.Retired, old)
						w.gone[r.kind.Name] = append(w.gone[r.kind.Name], &goneResource{key: r.r.Key, state: old})
					}
					r.state.Terraform = k.Type + "." + tfNamer(k.Type).assign(tfBase(r.r.Name, r.r.Key), r.r.Key)
				}
			}
			if k := r.kind.Crossplane; k != nil && r.state.Crossplane == "" {
				r.state.Crossplane = k8sNamer(k.Kind).assign(k8sBase(r.r.Name, r.r.Key), r.r.Key)
			}
			// Files the resource writes, by the provider's path.
			want := map[string]bool{}
			walkValues(r.r.Attributes, func(v any) {
				if f, ok := v.(sdk.FileValue); ok {
					want[f.File] = true
				}
			})
			if r.state.Files == nil {
				r.state.Files = map[string]string{}
			}
			for p := range r.state.Files {
				if !want[p] {
					delete(r.state.Files, p)
				}
			}
			for _, p := range sortedSet(want) {
				if old, ok := r.state.Files[p]; ok && !terraformLoaded(old) {
					if owner, dup := claimed[strings.ToLower(old)]; !dup || owner == r.r.Key {
						claimed[strings.ToLower(old)] = r.r.Key
						continue
					}
					// Another resource has this file: a new one for this one.
				}
				sp := safePath(p, r.r.Key)
				if files.taken(sp) {
					// Names differ by extension only: try the full path.
					ext := ""
					if i := strings.LastIndex(sp, "."); i > strings.LastIndex(sp, "/") {
						ext = sp[i:]
					}
					base := strings.TrimSuffix(sp, ext)
					sp = base + "-" + shortHash(r.r.Key) + ext
					for i := 2; files.taken(sp); i++ {
						sp = fmt.Sprintf("%s-%s%d%s", base, shortHash(r.r.Key), i, ext)
					}
				}
				files.reserve(sp)
				claimed[strings.ToLower(sp)] = r.r.Key
				r.state.Files[p] = sp
			}
		}
	}
	return workspaces, byKey, nil
}

func stateList(st *State) []*ResourceState {
	out := make([]*ResourceState, 0, len(st.Resources))
	for _, rs := range st.Resources {
		out = append(out, rs)
	}
	return out
}

func cloneState(prev *State, provider string) *State {
	st := NewState(provider)
	if prev == nil {
		return st
	}
	for k, v := range prev.Workspaces {
		c := *v
		st.Workspaces[k] = &c
	}
	for k, v := range prev.Resources {
		c := *v
		if v.Files != nil {
			c.Files = map[string]string{}
			for a, b := range v.Files {
				c.Files[a] = b
			}
		}
		if v.Secrets != nil {
			c.Secrets = map[string]string{}
			for a, b := range v.Secrets {
				c.Secrets[a] = b
			}
		}
		st.Resources[k] = &c
	}
	for _, v := range prev.Retired {
		c := *v
		st.Retired = append(st.Retired, &c)
	}
	for k, v := range prev.Hashes {
		if st.Hashes == nil {
			st.Hashes = map[string]string{}
		}
		st.Hashes[k] = v
	}
	for f, wss := range prev.Files {
		for ws, kinds := range wss {
			for k, paths := range kinds {
				st.setOwned(f, ws, k, append([]string(nil), paths...))
			}
		}
	}
	return st
}

func (r *Result) unit(format, ws, kind string) *unit {
	k := unitKey{format, ws, kind}
	u, ok := r.units[k]
	if !ok {
		u = &unit{files: map[string][]byte{}, secret: map[string]bool{}}
		r.units[k] = u
	}
	return u
}

func walkValues(v any, fn func(any)) {
	fn(v)
	switch t := v.(type) {
	case map[string]any:
		for _, e := range t {
			walkValues(e, fn)
		}
	case []any:
		for _, e := range t {
			walkValues(e, fn)
		}
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func partitionName(inv *sdk.Inventory, id string) string {
	for _, p := range inv.Partitions {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}

func describeWarning(w sdk.Warning) string {
	var where []string
	if w.Partition != "" {
		where = append(where, "partition "+w.Partition)
	}
	if w.Kind != "" {
		where = append(where, w.Kind)
	}
	if len(where) == 0 {
		return w.Message
	}
	return strings.Join(where, ", ") + ": " + w.Message
}
