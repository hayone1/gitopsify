resource "grafana_folder" "sre" {
  title = "SRE"
  uid   = "sre"
}

resource "grafana_folder" "kubernetes" {
  parent_folder_uid = grafana_folder.sre.uid
  title             = "Kubernetes"
  uid               = "sre-k8s"
}
