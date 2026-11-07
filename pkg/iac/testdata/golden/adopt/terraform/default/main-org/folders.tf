resource "grafana_folder" "sre" {
  title = "SRE"
  uid   = "sre"
}

resource "grafana_folder" "kubernetes" {
  parent_folder_uid = grafana_folder.sre.uid
  title             = "Kubernetes"
  uid               = "sre-k8s"
}

import {
  to = grafana_folder.sre
  id = "1:sre"
}

import {
  to = grafana_folder.kubernetes
  id = "1:sre-k8s"
}
