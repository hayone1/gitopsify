package iac

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

// Report summarises an export.
type Report struct {
	Provider   string
	Adopt      bool
	Formats    []string
	Instances  []InstanceReport
	Workspaces []WorkspaceReport
	// Warnings are what the provider couldn't read.
	Warnings []string
	// Skipped are objects the provider left out on purpose.
	Skipped []string
	// MissingSecrets are secret attributes left out of the output.
	MissingSecrets []string
	// Variables are Terraform variables that need values.
	Variables []string
	// Secrets are Kubernetes Secrets that need to be created.
	Secrets []string
	// Large are managed resources above LargeObject.
	Large []string
	// Notes are what providers said about particular objects.
	Notes []string
}

// InstanceReport is one instance.
type InstanceReport struct {
	Name, Target string
}

// WorkspaceReport is one workspace.
type WorkspaceReport struct {
	Title, Dir, Namespace string
	Counts                map[string]int
	// Gone counts resources exported before that aren't any more.
	Gone int
	// Incomplete lists kinds that weren't read completely ("*": all).
	Incomplete []string
}

// Total is the number of resources exported.
func (r *Report) Total() int {
	n := 0
	for _, w := range r.Workspaces {
		for _, c := range w.Counts {
			n += c
		}
	}
	return n
}

// GoneTotal is the number of resources no longer exported.
func (r *Report) GoneTotal() int {
	n := 0
	for _, w := range r.Workspaces {
		n += w.Gone
	}
	return n
}

// Gitignore is the .gitignore written at the root of an export.
const Gitignore = gitignoreHeader + `
.secrets/
*` + SecretTFVarsSuffix + `
.terraform/
*.tfstate
*.tfstate.*
crash.log
`

func renderReport(res *Result, in Input) {
	if contains(in.Options.Formats, config.FormatCrossplane) {
		finishCrossplane(res, in)
	}
	root := res.unit("", "", "")
	root.add(".gitignore", []byte(Gitignore), false)
	root.add("EXPORT_REPORT.md", reportMarkdown(&res.Report, in), false)
}

func reportMarkdown(r *Report, in Input) []byte {
	d := in.Description
	var b strings.Builder
	fmt.Fprintf(&b, "# %s export\n\n", r.Provider)
	mode := "**adopt**: the configuration manages the objects that were read (Terraform `import` blocks, Crossplane external names)."
	if !r.Adopt {
		mode = "**replicate**: the configuration creates the objects; nothing is imported."
	}
	fmt.Fprintf(&b, "Written by `gitopsify export %s` with provider %s %s. Mode: %s\n\n", d.Name, d.Name, d.Version, mode)

	b.WriteString("## Instances\n\n")
	for _, i := range r.Instances {
		fmt.Fprintf(&b, "- `%s`: %s\n", i.Name, firstNonEmpty(i.Target, "(not checked)"))
	}

	b.WriteString("\n## What was exported\n\n")
	kinds := map[string]bool{}
	for _, w := range r.Workspaces {
		for k := range w.Counts {
			kinds[k] = true
		}
	}
	cols := sortedSet(kinds)
	b.WriteString("| Workspace | Directory |")
	for _, k := range cols {
		b.WriteString(" " + k + " |")
	}
	b.WriteString(" No longer exported |\n|---|---|")
	for range cols {
		b.WriteString("---:|")
	}
	b.WriteString("---:|\n")
	for _, w := range r.Workspaces {
		title := w.Title
		if len(w.Incomplete) > 0 {
			title += " ⚠ incomplete: " + strings.Join(w.Incomplete, ", ")
		}
		fmt.Fprintf(&b, "| %s | `%s` |", title, w.Dir)
		for _, k := range cols {
			fmt.Fprintf(&b, " %d |", w.Counts[k])
		}
		fmt.Fprintf(&b, " %d |\n", w.Gone)
	}
	if r.GoneTotal() > 0 {
		b.WriteString("\nObjects no longer exported (deleted in the tool, or out of the selection) get a `removed` block with `destroy = false` in Terraform: Terraform stops managing them and never deletes them.")
		if contains(r.Formats, config.FormatCrossplane) {
			if r.Adopt {
				b.WriteString(" Their Crossplane objects are removed from the tree; with the default management policies (no `Delete`), pruning them leaves the tool's objects alone.")
			} else {
				b.WriteString(" Their Crossplane objects are removed from the tree; the default management policies of a replica include `Delete`, so a GitOps tool that prunes them deletes the objects in the instance they manage.")
			}
		}
		b.WriteString("\n")
	}

	section := func(title, intro string, items []string) {
		if len(items) == 0 {
			return
		}
		sort.Strings(items)
		fmt.Fprintf(&b, "\n## %s\n\n%s\n\n", title, intro)
		for _, i := range items {
			fmt.Fprintf(&b, "- %s\n", i)
		}
	}
	section("Could not be read", "The provider couldn't read these, so the previous export's files for them were kept. Fix the cause and export again.", r.Warnings)
	section("Left out on purpose", "These objects are managed some other way, or can't be expressed as configuration.", r.Skipped)
	section("Secrets left out", "These secret values aren't in the output. Add them by hand if the configuration should manage them; until then the first plan stays clean.", r.MissingSecrets)
	section("Terraform variables to set", "These secrets are Terraform variables without values. Set them (for example as environment variables) before `terraform plan`, or export again with `--secret-values local` to have them written to git-ignored `*"+SecretTFVarsSuffix+"` files.", r.Variables)
	section("Kubernetes Secrets to create", "The Crossplane objects reference these Secrets. Create them before applying, or export again with `--secret-values local` to have them written under `.secrets/`.", r.Secrets)
	section("Notes", "What the provider says about particular objects.", r.Notes)
	section("Large objects", "These Crossplane objects are big (Crossplane stores them twice: in spec and in status). Apply with server-side apply; objects above about 1.5 MiB can't be stored.", r.Large)

	b.WriteString("\n## Next steps\n\n")
	if contains(r.Formats, config.FormatTerraform) {
		b.WriteString("Terraform: see [terraform/README.md](terraform/README.md).\n\n")
	}
	if contains(r.Formats, config.FormatCrossplane) {
		b.WriteString("Crossplane: see [crossplane/README.md](crossplane/README.md).\n\n")
	}
	b.WriteString("Commit everything except what `.gitignore` lists. Export again with the same command (or `gitopsify export " + d.Name + " --no-input` with the saved `gitopsify-" + d.Name + ".yaml`) to pick up changes made in the tool.\n")
	return []byte(b.String())
}

func secretEnvs(d *sdk.Description) []string {
	var out []string
	for _, s := range d.Settings {
		if s.Secret && s.Env != "" {
			out = append(out, "`"+s.Env+"`")
		}
	}
	return out
}

func terraformReadme(res *Result, in Input) []byte {
	d := in.Description
	var dirs []string
	for _, w := range res.State.Workspaces {
		dirs = append(dirs, w.Dir)
	}
	sort.Strings(dirs)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s in Terraform\n\nOne root module per workspace (instance / %s), each with its own state:\n\n", d.Title, d.Partition.Singular)
	for _, dir := range dirs {
		fmt.Fprintf(&b, "- [`%s`](%s)\n", dir, dir)
	}
	b.WriteString("\nNeeds Terraform 1.7 or OpenTofu 1.10, or later (for `removed` blocks with `lifecycle`).\n\n## Plan and apply\n\n")
	if envs := secretEnvs(d); len(envs) > 0 {
		fmt.Fprintf(&b, "Credentials come from the environment (%s), as the %s provider reads them; set them for the instance whose workspaces you run.\n\n", strings.Join(envs, ", "), d.Terraform.Source)
	}
	b.WriteString("```sh\ncd " + firstNonEmpty(first(dirs), "<workspace>") + "\nterraform init\nterraform plan     # adopting: only imports, no changes\nterraform apply\n```\n\n")
	if in.Options.Adopt {
		b.WriteString("Each `<kind>.tf` ends with `import` blocks for the objects that were read. After the first apply they do nothing; you can delete them.\n\n")
	}
	b.WriteString("Secrets are sensitive variables. With `--secret-values local` their values are in `*" + SecretTFVarsSuffix + "` files next to the configuration, which Terraform loads automatically and Git ignores; otherwise set them as `TF_VAR_<name>` (EXPORT_REPORT.md lists them).\n\n")
	b.WriteString("State is local unless you set `output.terraform.backend` in `gitopsify-" + d.Name + ".yaml` (for example `{type: s3, settings: {bucket: my-state, key: \"" + d.Name + "/{workspace}/terraform.tfstate\", region: eu-central-1}}`) and export again.\n")
	return []byte(b.String())
}

func crossplaneReadme(res *Result, in Input, wsDirs []string) []byte {
	d := in.Description
	xp := d.Crossplane
	var b strings.Builder
	fmt.Fprintf(&b, "# %s in Crossplane\n\nManaged resources for %s (`%s`), one Kustomization and namespace per workspace (instance / %s). Needs Crossplane 2.0 or later (namespaced managed resources).\n\n", d.Title, xp.Name, xp.Package, d.Partition.Singular)
	b.WriteString("## 1. Install the provider\n\n```sh\nkubectl apply -k setup/\nkubectl wait provider.pkg.crossplane.io/" + xp.Name + " --for=condition=Healthy --timeout=10m\n# The provider can report Healthy before all its CRDs are served:\nkubectl wait crd --all --for=condition=Established --timeout=5m\n```\n\n")
	b.WriteString("`setup/` holds the Provider package. Apply it first: the " + xp.ProviderConfig.Kind + " of each workspace and the managed resources use kinds that exist only once the provider runs.\n\n")
	b.WriteString("## 2. Credentials\n\nEach " + xp.ProviderConfig.Kind + " reads its credentials from a Secret in `" + CredentialsNamespace + "`:\n\n")
	var keys []string
	for _, s := range d.Settings {
		if s.Crossplane != "" {
			keys = append(keys, s.Crossplane)
		}
	}
	insts := map[string]bool{}
	for _, w := range res.State.Workspaces {
		if inst, _, ok := strings.Cut(w.Dir, "/"); ok {
			insts[inst] = true
		}
	}
	b.WriteString("```sh\n")
	for _, inst := range sortedSet(insts) {
		var parts []string
		for _, s := range d.Settings {
			if s.Crossplane == "" {
				continue
			}
			v := "<" + s.Name + ">"
			if s.Secret && s.Env != "" {
				v = `'"$` + s.Env + `"'`
			}
			parts = append(parts, fmt.Sprintf(`"%s":"%s"`, s.Crossplane, v))
		}
		fmt.Fprintf(&b, "kubectl -n %s create secret generic %s \\\n  --from-literal=credentials='{%s}'\n", CredentialsNamespace, credentialsSecret(d, inst), strings.Join(parts, ","))
	}
	b.WriteString("```\n\n")
	if in.Options.SecretValues == config.SecretValuesLocal {
		b.WriteString("Or apply the ones gitopsify wrote: `kubectl apply -f ../.secrets/crossplane/setup/`, and the Secrets of the managed resources: `kubectl apply -R -f ../.secrets/crossplane/`.\n\n")
	}
	b.WriteString("## 3. Apply\n\n```sh\nkubectl apply --server-side -k .\nkubectl get managed -A\n```\n\n")
	if in.Options.Adopt {
		b.WriteString("While `components/observe-only` is listed in `kustomization.yaml`, every managed resource only **observes** its object (`managementPolicies: [Observe]`): Crossplane reads what it adopts and changes nothing. When all of them are `READY` and `SYNCED`, compare `spec.forProvider` with `status.atProvider` if you like, then remove the component from the list and apply again. From then on Crossplane creates and updates, but never deletes, the objects (no `Delete` policy); add `Delete` in `gitopsify-" + d.Name + ".yaml` (`output.crossplane.managementPolicies`) when the configuration should own their lifecycle.\n\n")
	}
	b.WriteString("With Argo CD or Flux, point one application at `setup/` and a second one, which depends on it, at this directory, and use server-side apply.\n\nWorkspaces (each with its namespace, its " + xp.ProviderConfig.Kind + " and its managed resources):\n\n")
	for _, dir := range wsDirs {
		fmt.Fprintf(&b, "- [`%s`](%s)\n", dir, dir)
	}
	return []byte(b.String())
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}
