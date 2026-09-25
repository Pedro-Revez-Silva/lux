# The control host as cloud-init leaves it (packages only), for
# scripts/host-smoke.sh. systemd is PID 1; `aws` and `cloudflared` are fakes.
FROM debian:trixie

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl gnupg git openssh-client python3 e2fsprogs util-linux \
 && install -d -m 0755 /usr/share/postgresql-common/pgdg \
 && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main" > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postgresql-18 \
 && systemctl enable postgresql \
 && rm -rf /var/lib/apt/lists/*

COPY fake-aws /usr/local/bin/aws
COPY fake-cloudflared /usr/local/bin/cloudflared
COPY fake-s3.py /usr/local/lib/smoke/fake-s3.py

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
