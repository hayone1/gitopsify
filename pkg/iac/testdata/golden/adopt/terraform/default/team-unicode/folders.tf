resource "grafana_folder" "r_de10ae" {
  title = "Апп"
  uid   = "apps"
}

import {
  to = grafana_folder.r_de10ae
  id = "2:apps"
}
