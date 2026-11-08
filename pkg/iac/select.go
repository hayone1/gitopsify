package iac

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/sdk"
)

// InstanceWide is the partition segment of addresses and directories for
// objects that belong to no partition.
const InstanceWide = "_instance"

// PartitionSlug is how a partition appears in addresses.
func PartitionSlug(p *sdk.Partition) string {
	if p == nil {
		return InstanceWide
	}
	if s := slug(p.Name, "-"); s != "" {
		return s
	}
	return slug(p.ID, "-")
}

// Address is how people select a resource: <Kind>/<partition>/<name>, with
// the partition and name as slugs (titles can contain slashes).
func Address(inv *sdk.Inventory, r *sdk.Resource) string {
	var p *sdk.Partition
	for i := range inv.Partitions {
		if inv.Partitions[i].ID == r.Partition {
			p = &inv.Partitions[i]
		}
	}
	if p == nil && r.Partition != "" {
		p = &sdk.Partition{ID: r.Partition, Name: r.Partition}
	}
	name := slug(r.Name, "-")
	if name == "" {
		name = slug(r.Key, "-")
	}
	return r.Kind + "/" + PartitionSlug(p) + "/" + name
}

// MatchPartitions returns the IDs of the partitions matching any pattern (an
// ID, a name or its slug, or a glob over them); all of them for no patterns.
// It fails for a pattern that matches nothing, which is usually a typo.
func MatchPartitions(all []sdk.Partition, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		ids := make([]string, 0, len(all))
		for _, p := range all {
			ids = append(ids, p.ID)
		}
		return ids, nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, pat := range patterns {
		hit := false
		for i := range all {
			p := &all[i]
			if match.Glob([]string{pat}, p.ID) || match.Glob([]string{pat}, p.Name) || match.Glob([]string{pat}, PartitionSlug(p)) ||
				strings.EqualFold(pat, p.Name) {
				hit = true
				if !seen[p.ID] {
					seen[p.ID] = true
					ids = append(ids, p.ID)
				}
			}
		}
		if !hit {
			var names []string
			for _, p := range all {
				names = append(names, PartitionSlug(&p))
			}
			return nil, fmt.Errorf("no partition matches %q (there are: %s)", pat, strings.Join(names, ", "))
		}
	}
	return ids, nil
}

// Selection narrows an inventory.
type Selection struct {
	// Include keeps only resources whose address matches (plus what they
	// reference, unless NoDeps).
	Include []string
	// Exclude drops resources whose address matches.
	Exclude []string
	NoDeps  bool
}

// Select returns the resources of inv that the selection keeps, in their
// original order. Partitions, skipped objects and warnings are kept.
func Select(inv *sdk.Inventory, sel Selection) *sdk.Inventory {
	out := &sdk.Inventory{Partitions: inv.Partitions, Skipped: inv.Skipped, Warnings: inv.Warnings}
	byKey := map[string]*sdk.Resource{}
	for i := range inv.Resources {
		byKey[inv.Resources[i].Key] = &inv.Resources[i]
	}
	keep := map[string]bool{}
	var queue []string
	for i := range inv.Resources {
		r := &inv.Resources[i]
		if len(sel.Include) == 0 || match.Glob(sel.Include, Address(inv, r)) {
			keep[r.Key] = true
			queue = append(queue, r.Key)
		}
	}
	if len(sel.Include) > 0 && !sel.NoDeps {
		for len(queue) > 0 {
			k := queue[0]
			queue = queue[1:]
			for _, dep := range Dependencies(byKey[k]) {
				if _, ok := byKey[dep]; ok && !keep[dep] {
					keep[dep] = true
					queue = append(queue, dep)
				}
			}
		}
	}
	for i := range inv.Resources {
		r := inv.Resources[i]
		if keep[r.Key] && !match.Glob(sel.Exclude, Address(inv, &r)) {
			out.Resources = append(out.Resources, r)
		}
	}
	// Report only the skipped objects the selection would have kept.
	if len(sel.Include) > 0 || len(sel.Exclude) > 0 {
		out.Skipped = nil
		for _, s := range inv.Skipped {
			addr := Address(inv, &sdk.Resource{Kind: s.Kind, Partition: s.Partition, Name: s.Name})
			if (len(sel.Include) == 0 || match.Glob(sel.Include, addr)) && !match.Glob(sel.Exclude, addr) {
				out.Skipped = append(out.Skipped, s)
			}
		}
	}
	return out
}

// Dependencies returns the keys a resource references or depends on.
func Dependencies(r *sdk.Resource) []string {
	if r == nil {
		return nil
	}
	set := map[string]bool{}
	for _, d := range r.DependsOn {
		set[d] = true
	}
	for _, body := range []map[string]any{r.Attributes, r.Crossplane} {
		walkValues(body, func(v any) {
			if ref, ok := v.(sdk.RefValue); ok {
				set[ref.Ref] = true
			}
		})
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
