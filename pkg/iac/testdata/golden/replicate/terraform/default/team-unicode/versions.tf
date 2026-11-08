terraform {
  required_version = ">= 1.7.0"
  required_providers {
    grafana = {
      source  = "grafana/grafana"
      version = "~> 4.53"
    }
  }

  backend "s3" {
    bucket = "state"
    key    = "grafana/default/team-unicode.tfstate"
  }
}
