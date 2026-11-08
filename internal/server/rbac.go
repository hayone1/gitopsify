package server

import (
	"path"
	"slices"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
)

// rank orders roles: a higher role can do everything a lower one can.
func rank(role string) int {
	switch role {
	case config.RoleViewer:
		return 1
	case config.RoleOperator:
		return 2
	case config.RoleAdmin:
		return 3
	}
	return 0
}

// atLeast reports whether role is want or higher.
func atLeast(role, want string) bool { return rank(role) >= rank(want) && rank(want) > 0 }

// grant is a role, everywhere (no clusters) or on some clusters.
type grant struct {
	Role     string
	Clusters []string
}

// policy decides what people may do.
type policy struct {
	local    bool
	def      string
	bindings []config.ServerBinding
}

// grants returns the user's grants. tokens are the grants of an API token
// (nil for people).
func (p *policy) grants(u *User, tokens []config.ServerToken) []grant {
	if u == nil {
		return nil
	}
	if p.local && u.Via == "local" {
		return []grant{{Role: config.RoleAdmin}}
	}
	if u.Via == "token" {
		var out []grant
		for _, t := range tokens {
			out = append(out, grant{Role: t.Role, Clusters: t.Clusters})
		}
		return out
	}
	var out []grant
	if p.def != "" {
		out = append(out, grant{Role: p.def})
	}
	for _, b := range p.bindings {
		if matchesAny(b.Users, u.Username) || matchesAny(b.Users, u.Email) || slices.ContainsFunc(u.Groups, func(g string) bool { return matchesAny(b.Groups, g) }) {
			out = append(out, grant{Role: b.Role, Clusters: b.Clusters})
		}
	}
	return out
}

func matchesAny(patterns []string, v string) bool {
	if v == "" {
		return false
	}
	for _, p := range patterns {
		if strings.EqualFold(p, v) {
			return true
		}
		if ok, _ := path.Match(p, v); ok && strings.ContainsAny(p, "*?[") {
			return true
		}
	}
	return false
}

// roleOn is the highest role the grants give on a cluster.
func roleOn(gs []grant, cluster string) string {
	best := ""
	for _, g := range gs {
		if len(g.Clusters) > 0 && !matchesAny(g.Clusters, cluster) {
			continue
		}
		if rank(g.Role) > rank(best) {
			best = g.Role
		}
	}
	return best
}

// serverRole is the highest role granted everywhere: what server-wide
// actions (the audit log, installing providers) check.
func serverRole(gs []grant) string {
	best := ""
	for _, g := range gs {
		if len(g.Clusters) == 0 && rank(g.Role) > rank(best) {
			best = g.Role
		}
	}
	return best
}

// anyRole is the highest role on any cluster: whether someone can use the
// UI at all.
func anyRole(gs []grant) string {
	best := ""
	for _, g := range gs {
		if rank(g.Role) > rank(best) {
			best = g.Role
		}
	}
	return best
}

// relevantGroups keeps the groups some binding names, so sessions stay small
// even for people in hundreds of groups.
func (p *policy) relevantGroups(groups []string) []string {
	var out []string
	for _, g := range groups {
		for _, b := range p.bindings {
			if matchesAny(b.Groups, g) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}
