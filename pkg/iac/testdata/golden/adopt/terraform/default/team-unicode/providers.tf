# default: Grafana 12.1.0 at http://grafana:3000
# Credentials come from the environment: GRAFANA_AUTH (credentials).
provider "grafana" {
  org_id = 2
  url    = "http://grafana:3000"
}
