job "worker" {
  region      = var.region
  datacenters = var.datacenters
  namespace   = var.namespace
  type        = "service"

  group "worker" {
    count = var.count

    update {
      max_parallel      = 1
      health_check      = "checks"
      min_healthy_time  = "10s"
      healthy_deadline  = "5m"
      progress_deadline = "10m"
      auto_revert       = true
      canary            = 0
      stagger           = "30s"
    }

    migrate {
      max_parallel     = 1
      health_check     = "checks"
      min_healthy_time = "10s"
      healthy_deadline = "5m"
    }

    shutdown_delay = "20s"

    volume "logs" {
      type   = "host"
      source = var.host_volume
    }

    network {
      port "http" {}
      port "grpc" {}
      port "metrics" {}
    }

    service {
      name         = "worker-http"
      tags         = var.discovery_service_tags
      port         = "http"
      address_mode = "host"
      check {
        name     = "worker HTTP Check"
        type     = "http"
        path     = "/healthz"
        interval = "3s"
        timeout  = "1s"
      }
    }

    service {
      name         = "worker-grpc"
      tags         = var.discovery_service_tags
      port         = "grpc"
      address_mode = "host"
      check {
        name     = "worker gRPC TCP Check"
        type     = "tcp"
        interval = "3s"
        timeout  = "1s"
      }
    }

    service {
      name         = "worker-prom"
      tags         = concat(["prometheus"], var.consul_service_tags)
      port         = "metrics"
      address_mode = "host"
      check {
        name     = "worker Metrics Check"
        type     = "http"
        path     = "/metrics"
        interval = "3s"
        timeout  = "1s"
      }
    }

    task "worker" {
      driver = "docker"
      user   = "root"
      kill_signal  = "SIGTERM"
      kill_timeout = "30s"

      volume_mount {
        volume      = "logs"
        destination = "/app/logs"
      }

      config {
        image        = var.image
        network_mode = "host"
        force_pull   = false
      }

      env {
        TZ                            = "Asia/Shanghai"
        SERVICE_NAME                  = "worker"
        TARGET_SERVICE_NAME           = "gateway"
        TARGET_DISCOVERY_SERVICE_NAME = "gateway-grpc"
        APP_PORT                      = "${NOMAD_PORT_http}"
        GRPC_PORT                     = "${NOMAD_PORT_grpc}"
        METRICS_PORT                  = "${NOMAD_PORT_metrics}"
        INSTANCE_ID                   = "${NOMAD_ALLOC_ID}"
        CONSUL_HTTP_ADDR              = var.consul_http_addr
        APP_LOG_PATH                  = "/app/logs/worker/${NOMAD_ALLOC_ID}.log"
        PEER_REFRESH_INTERVAL_MS      = var.peer_refresh_interval_ms
        REPORT_INTERVAL_MS            = var.report_interval_ms
        MINIO_ENDPOINT                = var.minio_endpoint
        MINIO_ACCESS_KEY              = var.minio_access_key
        MINIO_SECRET_KEY              = var.minio_secret_key
        MINIO_BUCKET                  = var.minio_bucket
        MINIO_USE_SSL                 = var.minio_use_ssl
      }

      resources {
        cpu    = var.cpu
        memory = var.memory
      }
    }
  }
}

variable "region" {
  type = string
}

variable "datacenters" {
  type = list(string)
}

variable "namespace" {
  type    = string
  default = "default"
}

variable "image" {
  type = string
}

variable "consul_http_addr" {
  type    = string
  default = "http://127.0.0.1:8500"
}

variable "consul_service_tags" {
  type    = list(string)
  default = []
}

variable "discovery_service_tags" {
  type    = list(string)
  default = []
}

variable "count" {
  type    = number
  default = 5
}

variable "cpu" {
  type    = number
  default = 100
}

variable "memory" {
  type    = number
  default = 128
}

variable "peer_refresh_interval_ms" {
  type    = string
  default = "5000"
}

variable "report_interval_ms" {
  type    = string
  default = "4000"
}

variable "minio_endpoint" {
  type    = string
  default = ""
}

variable "minio_access_key" {
  type    = string
  default = ""
}

variable "minio_secret_key" {
  type    = string
  default = ""
}

variable "minio_bucket" {
  type    = string
  default = "login-snapshots"
}

variable "minio_use_ssl" {
  type    = string
  default = "false"
}

variable "host_volume" {
  type    = string
  default = "logs"
}
