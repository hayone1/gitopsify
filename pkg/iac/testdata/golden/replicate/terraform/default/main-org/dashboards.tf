resource "grafana_dashboard" "home" {
  config_json = file("${path.module}/dashboards/general/home.json")
}

resource "grafana_dashboard" "kubernetes_overview" {
  config_json = file("${path.module}/dashboards/kubernetes/overview.json")
  folder      = grafana_folder.kubernetes.uid
}
