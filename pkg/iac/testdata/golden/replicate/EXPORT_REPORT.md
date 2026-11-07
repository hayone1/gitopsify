# Grafana export

Written by `gitopsify export grafana` with provider grafana v0.1.0. Mode: **replicate**: the configuration creates the objects; nothing is imported.

## Instances

- `default`: Grafana 12.1.0 at http://grafana:3000

## What was exported

| Workspace | Directory | ContactPoint | Dashboard | DataSource | Folder | No longer exported |
|---|---|---:|---:|---:|---:|---:|
| default / Main Org. | `default/main-org` | 1 | 2 | 1 | 2 | 0 |
| default / Team Ünicode | `default/team-unicode` | 0 | 0 | 0 | 1 | 0 |

## Left out on purpose

These objects are managed some other way, or can't be expressed as configuration.

- Dashboard "Node exporter" (default / Main Org.): provisioned from files

## Secrets left out

These secret values aren't in the output. Add them by hand if the configuration should manage them; until then the first plan stays clean.

- DataSource "Prometheus" (default / Main Org.): secure_json_data_encoded (basicAuthPassword)
- DataSource "Prometheus" (default / Main Org.): secure_json_data_encoded: the tool doesn't return this secret

## Next steps

Terraform: see [terraform/README.md](terraform/README.md).

Crossplane: see [crossplane/README.md](crossplane/README.md).

Commit everything except what `.gitignore` lists. Export again with the same command (or `gitopsify export grafana --no-input` with the saved `gitopsify-grafana.yaml`) to pick up changes made in the tool.
