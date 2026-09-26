# The control host as cloud-init leaves it (base packages only; Postgres 18
# is the reconciler's to install), for scripts/host-smoke.sh. systemd is
# PID 1; `aws` and `cloudflared` are fakes, and the fake cloudflared at
# /usr/local/bin makes the reconciler skip that install.
FROM debian:trixie

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl gnupg git openssh-client python3 e2fsprogs util-linux \
 && rm -rf /var/lib/apt/lists/*

COPY fake-aws /usr/local/bin/aws
COPY fake-cloudflared /usr/local/bin/cloudflared
COPY fake-s3.py /usr/local/lib/smoke/fake-s3.py

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
