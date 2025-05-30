# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: BUSL-1.1

plugin_dir = "/opt/nomad/plugins"

client {
  enabled = true
  options = {
    "user.denylist" = "www-data"
  }
}

plugin "nomad-driver-podman" {
  config {
    volumes {
      enabled = true
    }
    auth {
      helper = "test.sh"
      config = "/etc/auth.json"
    }
  }
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

plugin "docker" {
  config {
    allow_privileged = true

    volumes {
      enabled = true
    }
  }
}

plugin "nomad-pledge-driver" {
  config {
    pledge_executable = "/usr/local/bin/pledge"
  }
}

plugin "nomad-driver-exec2" {
  config {
    unveil_defaults = true
    unveil_by_task  = true
    unveil_paths    = ["r:/etc/mime.types"]
  }
}
