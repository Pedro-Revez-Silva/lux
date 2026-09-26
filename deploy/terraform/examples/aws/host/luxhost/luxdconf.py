"""/etc/lux/luxd.toml, rendered from SSM infrastructure values and the
desired-state file's [luxd] table."""
import json
import socket

from .desired import Desired


def primary_ip() -> str:
    # Connecting a UDP socket sends nothing; it only selects the source
    # address the default route would use.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.connect(("10.255.255.255", 1))
        return s.getsockname()[0]


def _value(v) -> str:
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return repr(v)
    # JSON string escapes are valid TOML basic-string escapes.
    return json.dumps(v, ensure_ascii=False)


def render(infra: dict, region: str, desired: Desired, creds: dict, ip: str, runner_bin_dir: str) -> str:
    port = infra["luxd_port"]
    top = {
        "listen": f"0.0.0.0:{port}",
        "public_url": infra["public_url"],
        "runner_url": f"http://{ip}:{port}",
        "runner_bin_dir": runner_bin_dir,
        **desired.luxd,
    }
    team = infra["cf_access_team"]
    tables = {
        "database": {
            "url": f"postgres://lux_app:{creds['app_password']}@127.0.0.1:5432/{infra['db_name']}?sslmode=disable",
            "app_password": creds["app_password"],
        },
        "s3": {"bucket": infra["blob_bucket"], "region": region},
        **desired.luxd_tables,
        "console": {"auth": "cloudflare-access" if team else "key"},
        "console.cloudflare_access": {"team": team, "aud": infra["cf_access_aud"]},
    }
    lines = ["# Written by lux-reconcile from SSM and lux-host.toml; edits here are overwritten."]
    lines += [f"{k} = {_value(v)}" for k, v in top.items()]
    for name, kv in tables.items():
        lines += ["", f"[{name}]"]
        lines += [f"{k} = {_value(v)}" for k, v in kv.items()]
    return "\n".join(lines) + "\n"
