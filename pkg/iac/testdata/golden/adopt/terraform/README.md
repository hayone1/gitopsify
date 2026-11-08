# Grafana in Terraform

One root module per workspace (instance / organization), each with its own state:

- [`default/main-org`](default/main-org)
- [`default/team-unicode`](default/team-unicode)

Needs Terraform 1.7 or OpenTofu 1.10, or later (for `removed` blocks with `lifecycle`).

## Plan and apply

Credentials come from the environment (`GRAFANA_AUTH`), as the grafana/grafana provider reads them; set them for the instance whose workspaces you run.

```sh
cd default/main-org
terraform init
terraform plan     # adopting: only imports, no changes
terraform apply
```

Each `<kind>.tf` ends with `import` blocks for the objects that were read. After the first apply they do nothing; you can delete them.

Secrets are sensitive variables. With `--secret-values local` their values are in `*.secrets.auto.tfvars` files next to the configuration, which Terraform loads automatically and Git ignores; otherwise set them as `TF_VAR_<name>` (EXPORT_REPORT.md lists them).

State is local unless you set `output.terraform.backend` in `gitopsify-grafana.yaml` (for example `{type: s3, settings: {bucket: my-state, key: "grafana/{workspace}/terraform.tfstate", region: eu-central-1}}`) and export again.
