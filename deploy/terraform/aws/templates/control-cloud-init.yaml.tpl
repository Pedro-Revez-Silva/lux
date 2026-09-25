#cloud-config
# Bootstrap only: packages, the config repo checkout, and the reconcile
# timer. Everything else (Postgres volume and database, luxd.toml, the lux
# release, cloudflared, backups, the systemd units) is done by
# host/reconcile.py from the checkout, every 5 minutes.
hostname: ${hostname}

package_update: true
package_upgrade: false

packages:
  - curl
  - ca-certificates
  - gnupg
  - git
  - openssh-client
  - python3

write_files:
  - path: /etc/lux/host.json
    permissions: "0644"
    owner: root:root
    content: |
      ${host_json}

  # Same content the reconciler writes (host/luxhost/units.py), so its
  # first run finds nothing to change here.
  - path: /etc/systemd/system/lux-reconcile.timer
    permissions: "0644"
    content: |
      [Unit]
      Description=Reconcile the lux control host with its config repo every 5 minutes

      [Timer]
      OnBootSec=1min
      OnUnitActiveSec=5min
      AccuracySec=30s

      [Install]
      WantedBy=timers.target

  - path: /etc/systemd/system/lux-reconcile.service
    permissions: "0644"
    content: |
      [Unit]
      Description=Reconcile the lux control host with its config repo
      After=network-online.target
      Wants=network-online.target

      [Service]
      Type=oneshot
      Environment=PYTHONDONTWRITEBYTECODE=1
      ExecStart=/usr/bin/python3 ${host_dir}/reconcile.py

runcmd:
  # amazon-ssm-agent: not in the Debian archive, and the official Debian
  # Cloud Images don't ship it; Session Manager is the only shell access.
  - |
    if ! dpkg -s amazon-ssm-agent >/dev/null 2>&1; then
      curl -fsSL https://s3.${region}.amazonaws.com/amazon-ssm-${region}/latest/debian_arm64/amazon-ssm-agent.deb -o /tmp/amazon-ssm-agent.deb
      dpkg -i /tmp/amazon-ssm-agent.deb
      systemctl enable --now amazon-ssm-agent
    fi
  # Postgres 18 via PGDG: trixie ships 17 (docs/operations.md says tested
  # on 18).
  - install -d -m 0755 /usr/share/postgresql-common/pgdg
  - curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
  - sh -c 'echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main" > /etc/apt/sources.list.d/pgdg.list'
  - apt-get update
  - DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql-18 awscli
  - systemctl enable postgresql
  - mkdir -p /etc/cloudflared
  - curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64.deb -o /tmp/cloudflared.deb
  - dpkg -i /tmp/cloudflared.deb || apt-get install -f -y
  # The config repo; no deploy key parameter means a public repo. runcmd
  # entries run as one script, so set -e makes a failed clone end it:
  # without a checkout there is nothing to reconcile.
  - |
    set -e
    install -d -m 0700 /root/.ssh
    if [ -n "${key_param}" ]; then
      umask 077
      aws ssm get-parameter --name "${key_param}" --with-decryption --region "${region}" \
        --query Parameter.Value --output text > /root/.ssh/lux-config-deploy-key
      export GIT_SSH_COMMAND="ssh -i /root/.ssh/lux-config-deploy-key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/root/.ssh/known_hosts"
    fi
    if [ ! -d "${checkout}/.git" ]; then
      install -d -m 0755 "$(dirname "${checkout}")"
      git clone --branch "${repo_ref}" "${repo_url}" "${checkout}"
    fi
  - test -f "${host_dir}/reconcile.py" || exit 1
  - systemctl daemon-reload
  - systemctl enable --now lux-reconcile.timer
  - systemctl start --no-block lux-reconcile.service

final_message: "lux control host bootstrapped after $UPTIME seconds; see journalctl -u lux-reconcile"
