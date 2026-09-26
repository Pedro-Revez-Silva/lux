#cloud-config
# Bootstrap only: base packages, awscli (the deploy key is read from SSM
# before the clone), amazon-ssm-agent, the config repo checkout, and the
# reconcile timer. Everything else (Postgres 18 and cloudflared packages,
# Postgres volume and database, luxd.toml, the lux release, backups, the
# systemd units) is done by host/reconcile.py from the checkout, every 5
# minutes.
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
  # Installed by the packages module, before runcmd's clone needs it and
  # before the reconcile timer exists, so nothing contends for the dpkg lock.
  - awscli

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
      # oneshot has no start timeout by default: a hung run would hold the
      # lock and stop the timer from ever starting another.
      TimeoutStartSec=20min
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
  # The config repo; no deploy key parameter means a public repo. runcmd
  # entries run as one script, so set -e makes a failed clone end it:
  # without a checkout there is nothing to reconcile.
  - |
    set -e
    install -d -m 0700 /root/.ssh
    if [ -n "${key_param}" ]; then
      # Subshell: umask 077 must not reach the clone, which postgres
      # (the backup unit) has to be able to read.
      (umask 077
       aws ssm get-parameter --name "${key_param}" --with-decryption --region "${region}" \
         --query Parameter.Value --output text > /root/.ssh/lux-config-deploy-key)
      export GIT_SSH_COMMAND="ssh -i /root/.ssh/lux-config-deploy-key -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/root/.ssh/known_hosts"
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
