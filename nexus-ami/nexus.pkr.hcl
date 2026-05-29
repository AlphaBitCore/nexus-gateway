# Nexus Gateway — Packer template for the AMI / appliance form factor.
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md
#
# Build:    cd nexus-ami && packer init . && packer build nexus.pkr.hcl
# Variables: pass via -var "nexus_version=0.1.0" or set NEXUS_VERSION env.

packer {
  required_plugins {
    amazon = {
      version = ">= 1.3.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "nexus_version" {
  type    = string
  default = "0.1.0"
}

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "instance_type" {
  type    = string
  # m5.4xlarge (16 vCPU / 64 GB) needed because valkey-search 1.x vendors
  # gRPC + Protobuf + Abseil + ICU as submodules; template-heavy parallel C++
  # compile is heap-hungry per translation unit. A 2026-05-28 build on
  # t3.2xlarge (32 GB) was OOM-killed silently mid-ICU-compile after 11
  # minutes — kernel OOM-killer leaves no trace in build logs (sshd dies
  # before the script can write stderr). 64 GB gives the headroom the 32 GB
  # tier was supposed to but no longer does.
  default = "m5.4xlarge"
}

variable "root_volume_size_gb" {
  type    = number
  default = 30           # Postgres + Valkey + NATS file store + log headroom.
}

source "amazon-ebs" "nexus" {
  region        = var.aws_region
  instance_type = var.instance_type

  ami_name        = "nexus-gateway-${var.nexus_version}-{{timestamp}}"
  # ami_description is ASCII-only: AWS ModifyImageAttribute rejects non-ASCII
  # (we hit this on 2026-05-28: em dash U+2014 → InvalidParameterValue; the
  # AMI was deregistered and the snapshot deleted at the end of the build).
  ami_description = "Nexus Gateway ${var.nexus_version} - single-instance AI traffic gateway appliance (OSS, Apache 2.0)"

  source_ami_filter {
    filters = {
      name                = "al2023-ami-2023.*-x86_64"
      virtualization-type = "hvm"
      root-device-type    = "ebs"
    }
    owners      = ["amazon"]
    most_recent = true
  }

  ssh_username = "ec2-user"

  launch_block_device_mappings {
    device_name = "/dev/xvda"
    volume_size = var.root_volume_size_gb
    volume_type = "gp3"
    delete_on_termination = true
  }

  ami_block_device_mappings {
    device_name = "/dev/xvda"
    volume_size = var.root_volume_size_gb
    volume_type = "gp3"
  }

  tags = {
    Name          = "nexus-gateway-${var.nexus_version}"
    Product       = "Nexus Gateway"
    Version       = var.nexus_version
    BuildToolchain = "packer+al2023"
  }
}

build {
  name    = "nexus-gateway-ami"
  sources = ["source.amazon-ebs.nexus"]

  # Upload artifacts.tar.gz (built by build.sh) as a single file. We avoid
  # uploading artifacts/ as a directory because Packer's file provisioner uses
  # recursive SCP — over slow links it silently drops individual files when
  # the connection blips, causing "missing binary" errors at install.sh time.
  # A single-file SCP is atomic: either the whole tarball lands or the
  # transfer errors loudly. install.sh extracts the tarball before doing
  # anything else.
  provisioner "file" {
    source      = "artifacts.tar.gz"
    destination = "/tmp/nexus-artifacts.tar.gz"
  }

  provisioner "shell" {
    execute_command = "sudo -E bash '{{.Path}}'"
    scripts = [
      "scripts/install.sh",
      "scripts/harden.sh",
    ]
  }
}
