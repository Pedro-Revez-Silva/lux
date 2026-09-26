# lux on AWS

Reusable Terraform for running lux (`luxd`, Postgres, spot runners) on AWS.
Nothing here names a company or account: an example root module
(`examples/aws/`) and `terraform.tfvars.example` show how to call it; a
separate private repo should hold the real values and state.

## What it builds

- **Network** (`aws/network.tf`): one VPC, public subnets in 2-3 AZs, no
  NAT gateway. Runners get public IPs (mirrors, git, model APIs) behind a
  security group with no inbound. The control host also gets a public IP
  (needed for outbound internet with no NAT), behind a security group
  with no inbound from the internet — only runners reach it, on luxd's
  port. An S3 gateway endpoint (free) keeps blob and backup traffic off
  the public internet.
- **Control host** (`aws/control.tf`): one `t4g.medium` running Debian 13
  (trixie) arm64, with `luxd` and Postgres 18 (PGDG, installed by the
  reconciler) both on it. Root and Postgres-data volumes are separate
  encrypted gp3 EBS volumes; the
  data volume has `prevent_destroy` so replacing the instance does not
  touch it. `lifecycle { ignore_changes = [ami, user_data] }` keeps a
  newer Debian AMI or an edited cloud-init template from triggering an
  unplanned replacement. cloud-init is only a bootstrap: it installs
  base packages (git, python3, awscli), clones the operator's **config
  repo** and starts `lux-reconcile.timer`, which runs
  `host/reconcile.py` from that checkout every 5 minutes. The reconciler
  does everything else (installing Postgres 18 and cloudflared, the
  Postgres volume and database, `luxd.toml`, the lux release, the
  cloudflared token, the daily `pg_dump -Fc` to S3, the systemd units)
  from the repo's `host/lux-host.toml` plus infrastructure values Terraform
  writes to SSM — see "Config repo" below. Shell access is SSM Session
  Manager only: no SSH, no key pairs — cloud-init installs the regional
  `amazon-ssm-agent` `.deb`, which the Debian Cloud Image doesn't ship
  (UNVERIFIED: no AWS credentials available to confirm this against a
  live account — see `.fix-report.md`).
- **Runners** (`aws/runners.tf`): one launch template per pool (an
  `arm64`/`m8g.2xlarge` spot pool by default; an `amd64`/`m7i.2xlarge` one
  behind a variable). Runners boot **Fedora CoreOS, stable stream**
  (looked up by architecture from Fedora's own AWS account, verified
  against `aws ec2 describe-images`) — not a custom AMI. FCOS already
  ships Podman 5.8, netavark and nftables, so **nothing installs at
  boot**: luxd passes an Ignition config as user data straight to
  `RunInstances` (the launch templates set none), and a runner is usually
  registered with luxd in under a minute — the only network activity is
  luxd's ~20 MB runner binaries coming from the control host over the
  private network (see "Cost" below: this makes the control host the
  upload bottleneck for the whole deployment). `update_default_version =
  true` on the launch template, so a new FCOS AMI, volume size or IOPS
  change (luxd always launches `$Default`) takes effect on the very next
  runner it starts, not only on templates created after the change.
  Launching a stock Fedora/Ubuntu/AL2023 AMI instead
  (`image = "custom"`, `userData = "script"`, which installs Podman if
  missing) or a prebuilt image (`userData = "env"`) both stay available
  per pool if FCOS does not fit; pick `"script"` for a distro you already
  trust without building an AMI, `"env"` only for an AMI that already has
  `lux-runner` on it. Root volume is gp3, sized by a variable (default
  100 GB — FCOS keeps container storage on `/var`, on this same volume),
  at the gp3 baseline IOPS/throughput (3000/125: no provisioned-IOPS
  surcharge). No SSH, no instance profile: Fedora CoreOS has no SSM agent,
  and runners never hold S3 credentials, so there is nothing to attach
  one for; reach a runner's containers through lux itself
  (`lux exec`/`lux attach`), and debug a boot failure with
  `aws ec2 get-console-output`.
- **IAM** (`aws/iam.tf`): the control host's instance role only —
  `ec2:RunInstances` scoped to the runner launch templates/subnets/SG
  (including spot-instances-request, for spot pools) and only from a
  runner launch template (`ec2:LaunchTemplate` on the instance),
  `ec2:CreateTags` on create, `ec2:TerminateInstances` conditioned on
  `lux:managed=true` and a `lux:host` tag (set only by luxd at launch; the
  control host carries no `lux:*` tag, and a postcondition refuses one
  arriving through `default_tags`), `ec2:DescribeInstances`, S3 on its own
  buckets, and `ssm:GetParameter(s)` under its own prefix, on the tunnel
  token's parameter, and on the config repo deploy key's parameter when
  one is set — nothing else. The SSM agent gets an inline copy of
  `AmazonSSMManagedInstanceCore` without its account-wide
  `ssm:GetParameter(s)` (and `ssm:GetManifest`), not the managed policy
  itself. Also creates the `AWSServiceRoleForEC2Spot`
  service-linked role, needed before a fresh account's first spot
  launch, behind `create_spot_service_linked_role` (default true — set
  to false, and `terraform import` it, on an account that already has
  it). Postgres passwords are generated and kept on the control host
  itself (`/root/.lux-*`), never written to SSM or Terraform state. No
  runner instance profile, so no `iam:PassRole` either (see "Runners"
  above).
- **S3** (`aws/s3.tf`): a blob bucket (snapshots/output/artifacts) and a
  Postgres backup bucket. Both block public access and use SSE-KMS; the
  backup bucket is versioned with a lifecycle expiry (default 30 days).
- **SSM** (`aws/ssm.tf`, `aws/control.tf`): infrastructure values the
  reconciler re-reads on every run (`<prefix>/public_url`,
  `cf_access_team`, `cf_access_aud`, `blob_bucket`, `backup_bucket`,
  `db_name`, `luxd_port`, `pg_data_volume_id`, `tunnel_token_parameter`,
  `config_repo_url`, `config_repo_ref`, `config_repo_deploy_key_parameter`),
  and the Cloudflare Tunnel token, written by Terraform only when
  `manage_cloudflare_tunnel_token = true` (otherwise expected to already
  exist). No lux version: that is in the config repo.
- **Cloudflare** (`cloudflare/`, optional): a Tunnel to the control host
  (`http://localhost:7070`), its DNS record, and two Access applications:
  one gating the console (bare hostname) to the given emails/domains,
  and one — on by default (`enable_api_bypass`) — with a `bypass`
  decision scoped to `/v1/*` and `/runner/*`, since those paths
  authenticate with lux API keys and runner tokens that luxd itself
  checks (see the module's comment for how Access resolves the overlap
  between the two applications).

## Cost (eu-north-1, monthly, on-demand unless noted)

| Item | Rate | Monthly |
| --- | --- | --- |
| Control host, `t4g.medium` | $0.0344/h | ~$25 |
| Control root volume, 20 GB gp3 | $0.096/GB-mo | ~$2 |
| Postgres data volume, 50 GB gp3 | $0.096/GB-mo | ~$5 |
| Runner, `m8g.2xlarge` spot, 1 warm | ~$0.10-0.13/h | ~$73-95 |
| Runner, `m8g.2xlarge` on-demand | $0.38/h | ~$277 (only while running) |
| Runner root volume, 100 GB gp3, while running | $0.096/GB-mo | ~$10 |
| S3 (blobs + backups) | ~$0.023/GB-mo + requests | usage-dependent, typically a few $ |
| S3 gateway endpoint | free | $0 |
| Data transfer out (runners, tunnel) | $0.09/GB after 1 GB free | usage-dependent |
| Public IPv4, control host + each running runner | $0.005/h each | ~$3.65/mo per address |
| Cross-AZ transfer (runners in a 2nd+ AZ, to the control host) | $0.01/GB each direction | usage-dependent; only with `az_count` > 1 and runners actually placed there |

A single-control-host, one-warm-runner deployment runs roughly **$100-130/month**
on spot, dominated by the control host and the one warm runner, plus
~$7/mo for the two public IPv4 addresses; it drops toward $30/month with
`--warm 0` and pools scaled to zero when idle (`lux pools set ... --min 0
--warm 0`). No NAT gateway (~$33/mo saved), no DynamoDB (S3 native
locking), no idle EC2 beyond the control host and whatever `--warm`
keeps ready.

`credit_specification { cpu_credits = "standard" }` is set on the
control host (`control.tf`) rather than t4g's `unlimited` default:
`unlimited` bills sustained load above the 20% baseline as surplus
credits with no cap, while `standard` throttles CPU at the baseline
instead — cheap and predictable, but the tradeoff is that a control
host under sustained heavy load (many concurrent runs, a busy Postgres)
throttles rather than bursts. Switch back to `unlimited` if that
throttling shows up.

luxd is the upload bottleneck for the whole deployment: every snapshot
and artifact goes runner → luxd → S3, through a single `t4g.medium`
whose baseline network bandwidth is ~0.25 Gbps (it bursts higher, but
not indefinitely). Adding more runner capacity does not add more upload
throughput; only a bigger control host instance type does.

## Bootstrap the state bucket

Terraform's own state needs an S3 bucket that exists before `terraform
init` can point at it (the backend block in `examples/aws/versions.tf` is
a partial config on purpose). Create it once, by hand or with a tiny
separate Terraform run, with versioning and SSE enabled:

```bash
aws s3api create-bucket --bucket <your-state-bucket> --region eu-north-1 \
  --create-bucket-configuration LocationConstraint=eu-north-1
aws s3api put-bucket-versioning --bucket <your-state-bucket> \
  --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket <your-state-bucket> \
  --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms"}}]}'
```

Then, in your private repo (not this one), point at it:

```bash
terraform init \
  -backend-config="bucket=<your-state-bucket>" \
  -backend-config="key=lux/terraform.tfstate" \
  -backend-config="region=eu-north-1" \
  -backend-config="use_lockfile=true"
```

`use_lockfile = true` is Terraform's native S3 locking (>= 1.10): no
DynamoDB table needed.

## Config repo

The control host is driven by a Git repo the operator owns (private or
public). `examples/aws/` is the template for one: copy it as the repo's
content. It holds

- the Terraform root that calls `aws/` and `cloudflare/` (infrastructure
  only, applied by hand);
- `host/`: the reconciler (`reconcile.py`, `backup.py`, `luxhost/`) and
  its tests;
- `host/lux-host.toml`: the desired state — `lux_version`, the release
  repo and luxd operator settings. Schema in `examples/aws/host/README.md`.

The module takes `config_repo_url` (SSH or HTTPS), `config_repo_ref`
(default `main`), `config_repo_path` (the subdirectory holding `host/`,
empty for the repo root) and `config_repo_deploy_key_parameter` (the name
of an SSM SecureString with a read-only SSH deploy key; empty for a public
repo). Create the key parameter outside Terraform, so the key never
enters state:

```bash
ssh-keygen -t ed25519 -N '' -C lux-control-host -f deploy_key
# add deploy_key.pub to the repo as a read-only deploy key, then:
aws ssm put-parameter --name /acme/lux-config-deploy-key --type SecureString \
  --value file://deploy_key
rm deploy_key deploy_key.pub
```

Every 5 minutes the host fetches `config_repo_ref`, hard-resets its
checkout to it and reconciles; a change to the host code re-executes the
new reconciler within the same run. A merged PR is the deploy.
`journalctl -u lux-reconcile` shows one summary line per run.

## First deploy

1. Create the config repo from `examples/aws/` (above). Copy
   `terraform.tfvars.example` to `terraform.tfvars` there and fill in the
   Cloudflare account/zone ids, allowed emails/domains, `public_url` and
   the `config_repo_*` values; set `lux_version` in `host/lux-host.toml`
   and push it to `config_repo_ref`.
2. `terraform init` (with your backend config), `terraform plan`,
   `terraform apply`.
3. SSM into the control host (`aws ssm start-session --target
   <instance-id>`) and check `journalctl -u lux-reconcile`: the first run
   (at the end of cloud-init) installs Postgres 18 and cloudflared,
   prepares Postgres, installs `lux_version` and runs `luxd migrate`
   before starting luxd.
4. Create your first tenant and operator key (docs/operations.md):
   `luxd admin create-tenant --name ...`, `luxd admin
   create-operator-key`.
5. Paste the `lux pools set ...` command from `terraform output
   runner_pools_set_commands` (one per pool) so luxd can launch runners.

## Upgrading lux

Open a PR in the config repo changing `lux_version` in
`host/lux-host.toml` to the new release tag, and merge it; no Terraform
run. Within 5 minutes the reconciler downloads the release, verifies its
checksum, and runs `luxd migrate` with the new binary before touching
anything running. Only once that succeeds does it switch to the new
version and restart luxd, then poll `systemctl is-active` and `/health`
for about 30s; on any failure it restores the previous version's
symlinks, restarts luxd again and the run fails (`status=error` in the
journal; `systemctl status lux-reconcile` shows it), leaving the previous
version running. It retries every 5 minutes until the file changes.
Reverting the PR does not downgrade the database: `luxd migrate` has
already run.

### Upgrade notes: from the SSM version parameter to a config repo

- Removed: the module's `lux_version` and `lux_repo` variables, the
  `<ssm_prefix>/version` parameter and the `lux_version_parameter`
  output. The version is `lux_version` in `host/lux-host.toml`; the
  release repo is `release_repo` there.
- Added: required `config_repo_url`; optional `config_repo_ref`,
  `config_repo_path`, `config_repo_deploy_key_parameter`.
- `terraform apply` destroys the old `/lux/version` parameter and creates
  the new ones, but does not change a running control host's user_data
  (ignored). To move it, either replace it (`terraform apply
  -replace=module.aws.aws_instance.control`; the Postgres volume survives
  and a volume with a filesystem is never reformatted), or, over Session
  Manager, write `/etc/lux/host.json` as cloud-init would, clone the repo
  to `/var/lib/lux/config` and run `python3
  /var/lib/lux/config/host/reconcile.py` once. That run disables and
  deletes `lux-deploy.timer`/`.service` and the old scripts under
  `/usr/local/{lib/lux,sbin}`, and installs `lux-reconcile.timer`.

## Restore from a backup

Backups are `pg_dump -Fc` dumps in the backup bucket, named
`<db_name>-<UTC timestamp>.dump`. To restore onto a fresh or existing
control host:

```bash
aws s3 cp s3://<backup-bucket>/<name>.dump /tmp/restore.dump
sudo -u postgres pg_restore --clean --if-exists -d <db_name> /tmp/restore.dump
```

Run `luxd migrate` afterward if the backup predates the running luxd
version.

## Runner image and user data

Per pool (`runner_pools` in `aws/runners.tf`): `image = "fcos"` (default)
boots Fedora CoreOS and needs `user_data_format = "ignition"` (the only
format FCOS understands); `image = "custom"` with `ami_id` set boots any
other AMI, with `user_data_format = "script"` (a cloud-init shell script
that installs Podman only if missing — for a stock Fedora/Ubuntu/AL2023
AMI you trust but haven't customized) or `"env"` (plain `KEY=value`
lines — for an AMI that already has `lux-runner` baked in). The value
feeds straight into the `userData` field of the `lux pools set --template`
JSON that luxd reads.

## GCP and Azure

Not built yet. `aws/` is meant to be one sibling of `gcp/` and `azure/`
under `deploy/terraform/`, sharing the same shape (network, control,
runners, iam, storage) when those are added.
