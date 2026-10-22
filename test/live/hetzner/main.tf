# Throwaway k3s clusters on Hetzner Cloud for gitopsify's live tests. Driven
# by run.sh; every resource carries the gitopsify-e2e label.
terraform {
  required_version = ">= 1.5"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = ">= 1.48"
    }
  }
}

variable "hcloud_token" {
  type      = string
  sensitive = true
}

variable "run_id" {
  type = string
}

variable "clusters" {
  description = "One single-node k3s cluster per name."
  type        = list(string)
  default     = ["src", "argocd", "flux"]
}

variable "server_type" {
  type    = string
  default = "cx33"
}

variable "location" {
  type    = string
  default = "fsn1"
}

variable "ssh_public_key" {
  type = string
}

variable "install_k3s" {
  description = "Install k3s; false for a plain machine that runs tests."
  type        = bool
  default     = true
}

variable "label" {
  description = "Label every resource carries, for cleanup."
  type        = string
  default     = "gitopsify-e2e"
}

variable "allowed_cidr" {
  description = "The only address allowed to reach SSH and the API servers."
  type        = string
}

provider "hcloud" {
  token = var.hcloud_token
}

locals {
  labels = { (var.label) = "true", "run" = var.run_id }
}

resource "hcloud_ssh_key" "e2e" {
  name       = "${var.label}-${var.run_id}"
  public_key = var.ssh_public_key
  labels     = local.labels
}

resource "hcloud_firewall" "e2e" {
  name   = "${var.label}-${var.run_id}"
  labels = local.labels
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = [var.allowed_cidr]
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "6443"
    source_ips = [var.allowed_cidr]
  }
  rule {
    direction  = "in"
    protocol   = "icmp"
    source_ips = [var.allowed_cidr]
  }
}

resource "hcloud_server" "k3s" {
  for_each     = toset(var.clusters)
  name         = "${var.label}-${each.key}-${var.run_id}"
  server_type  = var.server_type
  location     = var.location
  image        = "ubuntu-24.04"
  ssh_keys     = [hcloud_ssh_key.e2e.id]
  firewall_ids = [hcloud_firewall.e2e.id]
  labels       = merge(local.labels, { "cluster" = each.key })
  # Docker Hub limits anonymous pulls per IP, and cloud IPs are shared: pull
  # through Google's mirror instead.
  user_data = var.install_k3s ? local.k3s_user_data : null
}

locals {
  k3s_user_data = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/rancher/k3s/registries.yaml
        content: |
          mirrors:
            docker.io:
              endpoint: ["https://mirror.gcr.io"]
    runcmd:
      - [sh, -c, 'IP=$(curl -fsS http://169.254.169.254/hetzner/v1/metadata/public-ipv4) && curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --tls-san $IP --write-kubeconfig-mode 600" sh -']
  EOT
}

output "ips" {
  value = { for k, s in hcloud_server.k3s : k => s.ipv4_address }
}
