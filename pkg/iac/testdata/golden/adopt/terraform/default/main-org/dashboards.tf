resource "grafana_dashboard" "home" {
  config_json = file("${path.module}/dashboards/general/home.json")
}

resource "grafana_dashboard" "kubernetes_overview" {
  config_json = file("${path.module}/dashboards/kubernetes/overview.json")
  folder      = grafana_folder.kubernetes.uid
}

import {
  to = grafana_dashboard.home
  id = "1:home"
}

import {
  to = grafana_dashboard.kubernetes_overview
  id = "1:k8s"
}
