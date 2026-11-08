resource "grafana_contact_point" "on_call" {
  name = "On call"

  email {
    addresses    = ["a@example.com"]
    single_email = false
  }

  slack {
    recipient = "#alerts"
    text      = <<EOT
{{ template "x" . }}
line two
EOT
    url       = var.contact_point_on_call_url
  }
}

variable "contact_point_on_call_url" {
  description = "Slack webhook URL of ContactPoint \"On call\""
  type        = string
  sensitive   = true
}

import {
  to = grafana_contact_point.on_call
  id = "1:On call"
}
