resource "grafana_data_source" "prometheus" {
  is_default = true
  json_data_encoded = jsonencode({
    httpMethod   = "POST"
    timeInterval = "$${interval}"
  })
  name = "Prometheus"
  type = "prometheus"
  uid  = "prom"
  url  = "http://prometheus:9090"
}
