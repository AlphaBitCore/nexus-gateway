---
updated: 2026-05-29
---

# AMI build (single-instance appliance)

How to build the AWS Marketplace AMI / single-instance appliance image. The
source-of-truth for everything in this guide is [`nexus-ami/README.md`](../../../nexus-ami/README.md);
the design rationale is captured in
[`docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`](../../developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md).

## When to use this

- Cutting a release for the AWS Marketplace listing.
- Producing an image for an on-prem customer that wants a single-VM install.
- Smoke-testing a Packer / install-script change before publishing.

## Prerequisites

- Go 1.25+, Node 20+, Packer 1.10+.
- AWS credentials with `AWS_PROFILE=<profile>` exporting EC2 + S3 + IAM
  permissions in `us-east-1`.
- A `t3.medium` or larger key pair on the target account if you intend to
  launch instances from the AMI after build.
- vCPU headroom: the build runs on an `m5.4xlarge` (16 vCPU). If the
  Standard-family quota is 16 and another instance is already running, stop
  it or request a quota bump first (otherwise Packer fails immediately with
  `VcpuLimitExceeded`).

## Build

```bash
cd nexus-ami
./build.sh                  # full pipeline (compile + stage + packer build, ~55 min)
./build.sh --skip-packer    # CI dry-run — stage only, skip the EC2 launch
```

A successful build prints the new AMI id (e.g. `ami-0xxxxxxxx`) and a
snapshot id.

## After the build

1. **Share with the Marketplace scanner** (account `679593333241`):

   ```bash
   aws ec2 modify-image-attribute --image-id <AMI> \
     --launch-permission "Add=[{UserId=679593333241}]" \
     --profile <profile> --region us-east-1
   aws ec2 modify-snapshot-attribute --snapshot-id <SNAP> \
     --create-volume-permission "Add=[{UserId=679593333241}]" \
     --profile <profile> --region us-east-1
   ```

2. **Trigger the AMI scan** in Partner Central → AMI Management Portal.

3. **Test the AMI**:

   ```bash
   aws ec2 run-instances --image-id <AMI> --instance-type t3.medium \
     --key-name <your-key> --associate-public-ip-address \
     --profile <profile> --region us-east-1
   # SSH in, then: sudo cat /var/log/nexus/admin-credentials.txt
   ```

   Two instances launched from the same AMI MUST have different admin
   passwords — that is the most important first-boot invariant.

## Common failure modes

| Symptom | Root cause | Fix |
|---|---|---|
| `VcpuLimitExceeded` immediately at `packer build` | Standard-family quota hit because another instance is running | Stop or terminate it, or request a quota raise |
| `Script disconnected unexpectedly` mid-Valkey compile | Build host OOM-killed sshd | Default is `m5.4xlarge`; do not lower |
| `InvalidParameterValue: Character sets beyond ASCII are not supported` at `Modifying attributes on AMI` | Non-ASCII in `ami_description` (e.g. em dash) | Keep `nexus.pkr.hcl` `ami_description` ASCII-only |
| First-boot completes but 4 nexus-* services stay `inactive` | Boot-order race — nexus-* tried to start before postgres was up | Already handled by `first-boot.sh`'s tail `kick` block |

## Iteration cadence

Plan a **monthly rebuild** to absorb AL2023 + Postgres + Valkey + NATS CVE
patches. `./build.sh` is the single command; wire it into a CI cron once
the AMI is stabilised.
