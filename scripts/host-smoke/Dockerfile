# The control host as cloud-init leaves it (base packages only; Postgres 18
# and cloudflared are the reconciler's to install), for scripts/host-smoke.sh.
# systemd is PID 1; `aws` is a fake. cloudflared is the real release .deb,
# installed by the reconciler; an apt hook (swap-cloudflared) replaces its
# binary with the fake before the unit first starts.
FROM debian:trixie

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl gnupg git openssh-client python3 e2fsprogs util-linux \
 && rm -rf /var/lib/apt/lists/*

COPY fake-aws /usr/local/bin/aws
COPY fake-cloudflared swap-cloudflared fake-s3.py /usr/local/lib/smoke/
COPY 99smoke-cloudflared /etc/apt/apt.conf.d/99smoke-cloudflared

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
